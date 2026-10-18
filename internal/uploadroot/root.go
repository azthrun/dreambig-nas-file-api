package uploadroot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrOutOfScope means a path is outside the Caller's Scope, or resolves (through a
// symlink) outside it or outside the Upload Root.
var ErrOutOfScope = errors.New("outside the Caller's Scope")

// ErrNotFound means nothing exists at a valid, in-Scope path.
var ErrNotFound = errors.New("not found")

// ErrNotFolder means a path that must be a folder is something else.
var ErrNotFolder = errors.New("not a folder")

// Root is the Upload Root. All filesystem access for Caller-supplied paths goes through
// it: every method first checks the Caller's Scope and symlinks, then works through an
// os.Root so that nothing, not even a symlink swapped in meanwhile, escapes the Upload
// Root.
type Root struct {
	real string // absolute, symlinks resolved
	os   *os.Root
}

// Open opens dir as the Upload Root. dir must be an existing folder.
func Open(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("upload root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("upload root: %w", err)
	}
	r, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("upload root: %w", err)
	}
	return &Root{real: real, os: r}, nil
}

// Close releases the Upload Root.
func (r *Root) Close() error { return r.os.Close() }

// EntryType is what kind of thing an Entry is.
type EntryType string

const (
	TypeFile    EntryType = "file"
	TypeFolder  EntryType = "folder"
	TypeSymlink EntryType = "symlink"
	TypeOther   EntryType = "other"
)

// Entry is one non-hidden item in a folder.
type Entry struct {
	Name    string
	Type    EntryType
	Size    int64
	ModTime time.Time
}

// ReadDir lists folder p for a Caller with the given Scope, sorted by name. Hidden
// Entries are never included. Symlinks are reported as such and not followed.
func (r *Root) ReadDir(scope, p Path) ([]Entry, error) {
	if err := r.check(scope, p); err != nil {
		return nil, err
	}
	f, err := r.os.Open(p.osRel())
	if err != nil {
		return nil, notFound(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, notFound(err)
	}
	if !info.IsDir() {
		return nil, ErrNotFolder
	}
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		if p.IsHiddenChild(de.Name()) {
			continue
		}
		fi, err := de.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("stat entry: %w", err)
		}
		entries = append(entries, entryOf(fi))
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

func entryOf(fi fs.FileInfo) Entry {
	e := Entry{Name: fi.Name(), ModTime: fi.ModTime().UTC()}
	switch m := fi.Mode(); {
	case m.IsDir():
		e.Type = TypeFolder
	case m.IsRegular():
		e.Type, e.Size = TypeFile, fi.Size()
	case m&fs.ModeSymlink != 0:
		e.Type = TypeSymlink
	default:
		e.Type = TypeOther
	}
	return e
}

// check refuses p unless it is within scope, and unless the part of it that exists,
// with symlinks resolved, is still within scope and names no Hidden Entry. Missing
// trailing segments are allowed (they will be created inside a checked folder).
func (r *Root) check(scope, p Path) error {
	if !p.Within(scope) {
		return ErrOutOfScope
	}
	resolved, err := r.resolveExisting(p)
	if err != nil {
		return err
	}
	if !resolved.Within(scope) {
		return fmt.Errorf("%w: resolves through a symlink to outside it", ErrOutOfScope)
	}
	return nil
}

// resolveExisting resolves symlinks in the longest existing prefix of p and returns the
// result as a Path (plus the missing suffix). A result outside the Upload Root or naming
// a Hidden Entry is refused.
func (r *Root) resolveExisting(p Path) (Path, error) {
	segs := p.segs
	for n := len(segs); n >= 0; n-- {
		abs := filepath.Join(append([]string{r.real}, segs[:n]...)...)
		real, err := filepath.EvalSymlinks(abs)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Path{}, fmt.Errorf("resolve path: %w", err)
		}
		rel, err := filepath.Rel(r.real, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return Path{}, fmt.Errorf("%w: resolves outside the Upload Root", ErrOutOfScope)
		}
		var head Path
		if rel != "." {
			head, err = ParseSegments(strings.Split(filepath.ToSlash(rel), "/"))
			if err != nil {
				return Path{}, fmt.Errorf("%w: resolves to %v", ErrOutOfScope, err)
			}
		}
		return Path{segs: append(head.segs, segs[n:]...)}, nil
	}
	return p, nil
}

func (p Path) osRel() string {
	if p.IsRoot() {
		return "."
	}
	return filepath.Join(p.segs...)
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return fmt.Errorf("open: %w", err)
}
