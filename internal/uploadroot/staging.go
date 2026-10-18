package uploadroot

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// stagingPerm is the Staging Area's mode: only the service reads it.
const stagingPerm = 0o700

// StagingDir creates the Staging Area if it is missing and returns its OS path. It
// lives directly under the Upload Root, on the same filesystem, so completing an Upload
// is a link, never a copy (ADR-0003). Anything other than a real folder there (e.g. a
// symlink) is refused.
func (r *Root) StagingDir() (string, error) {
	if err := r.os.Mkdir(StagingDirName, stagingPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create Staging Area: %w", err)
	}
	fi, err := r.os.Lstat(StagingDirName)
	if err != nil {
		return "", fmt.Errorf("Staging Area: %w", err)
	}
	if !fi.IsDir() {
		return "", errors.New("Staging Area is not a folder")
	}
	return filepath.Join(r.real, StagingDirName), nil
}

// StagedNames lists the names directly in the Staging Area, read through the Upload
// Root's os.Root.
func (r *Root) StagedNames() ([]string, error) {
	if _, err := r.StagingDir(); err != nil {
		return nil, err
	}
	f, err := r.os.Open(StagingDirName)
	if err != nil {
		return nil, fmt.Errorf("open Staging Area: %w", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("read Staging Area: %w", err)
	}
	return names, nil
}

// LstatStaged describes the Staging Area entry name (a bare name), without following
// a symlink.
func (r *Root) LstatStaged(name string) (fs.FileInfo, error) {
	rel, err := stagedRel(name)
	if err != nil {
		return nil, err
	}
	return r.os.Lstat(rel)
}

// stagedRel is the Upload Root-relative OS path of the Staging Area entry name, which
// must be a bare name.
func stagedRel(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("bad staged name %q", name)
	}
	return filepath.Join(StagingDirName, name), nil
}

// CheckNewFile refuses a new file at p for a Caller with the given Scope unless p
// passes the write-side path rules, is in Scope, and nothing exists at p yet. Anything
// already at p (even a dangling symlink or a folder), or a non-folder in place of one of
// its parents, is ErrConflict. Missing parents are fine: they are created at completion.
func (r *Root) CheckNewFile(scope, p Path) error {
	if err := p.CheckWritable(); err != nil {
		return err
	}
	if p.IsRoot() {
		return invalid("the Upload Root is a folder")
	}
	if _, err := r.check(scope, p); err != nil {
		return err
	}
	for n := 1; n < len(p.segs); n++ {
		fi, err := r.os.Stat(filepath.Join(p.segs[:n]...))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat parent: %w", err)
		}
		if !fi.IsDir() {
			return ErrConflict
		}
	}
	_, err := r.os.Lstat(p.osRel())
	switch {
	case err == nil:
		return ErrConflict
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("stat: %w", err)
	}
}

// PlaceStaged completes an Upload: it moves the Staging Area file staged (a bare name)
// to p for a Caller with the given Scope, creating missing parents, with modification
// time mtime. It never overwrites: the file is hard-linked into place, which fails if
// anything appeared at p meanwhile (ErrConflict); the caller then removes the staged
// name. The staged file's own data is the file at p afterwards, never a copy.
func (r *Root) PlaceStaged(scope, p Path, staged string, mtime time.Time) error {
	src, err := stagedRel(staged)
	if err != nil {
		return err
	}
	if err := r.CheckNewFile(scope, p); err != nil {
		return err
	}
	if parent := (Path{segs: p.segs[:len(p.segs)-1]}); !parent.IsRoot() {
		if _, err := r.MkdirAll(scope, parent); err != nil {
			return err
		}
	}
	if err := r.os.Chtimes(src, time.Time{}, mtime); err != nil {
		return fmt.Errorf("set mtime: %w", err)
	}
	err = r.os.Link(src, p.osRel())
	if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("link into place: %w", err)
	}
	return nil
}
