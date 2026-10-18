package uploadroot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
)

// walkBatch is how many entries Walk reads from a folder at a time, so memory stays
// bounded by tree depth rather than by folder or tree size.
const walkBatch = 256

// WalkFunc receives one entry of a Walk: its slash-separated path relative to the
// walked folder, and the Entry (Name is the last segment). Returning an error stops the
// walk, and Walk returns that error.
type WalkFunc func(rel string, e Entry) error

// Walk visits every non-hidden entry below folder p for a Caller with the given Scope,
// depth-first and in no particular order, streaming as it reads: no folder is read
// whole. Hidden Entries and their subtrees are skipped. Symlinks are reported as such
// and never followed. The walk stops with ctx's error once ctx is done.
func (r *Root) Walk(ctx context.Context, scope, p Path, fn WalkFunc) error {
	if err := r.check(scope, p); err != nil {
		return err
	}
	// Walk the folder p really is, so that Hidden Entries are recognized by where they
	// are (p may be a symlink to the Upload Root, whose Staging Area must stay hidden).
	p, err := r.resolveExisting(p)
	if err != nil {
		return err
	}
	f, err := r.os.Open(p.osRel())
	if err != nil {
		return notFound(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return notFound(err)
	}
	if !info.IsDir() {
		f.Close()
		return ErrNotFolder
	}
	w := walker{root: r, ctx: ctx, fn: fn}
	return w.dir(f, p.segs, "")
}

type walker struct {
	root *Root
	ctx  context.Context
	fn   WalkFunc
}

// dir walks the open folder f, at Upload Root segments segs and at rel below the walked
// folder, and closes f.
func (w *walker) dir(f *os.File, segs []string, rel string) error {
	defer f.Close()
	dir := Path{segs: segs}
	for {
		des, readErr := f.ReadDir(walkBatch)
		for _, de := range des {
			if err := w.ctx.Err(); err != nil {
				return err
			}
			if dir.IsHiddenChild(de.Name()) {
				continue
			}
			fi, err := de.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed meanwhile
			}
			if err != nil {
				return fmt.Errorf("stat entry: %w", err)
			}
			e := entryOf(fi)
			childRel := path.Join(rel, e.Name)
			if err := w.fn(childRel, e); err != nil {
				return err
			}
			if e.Type == TypeFolder {
				if err := w.child(segs, de.Name(), childRel, fi); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read folder: %w", readErr)
		}
	}
}

// child descends into the folder name inside segs, which was a real folder (not a
// symlink) when listed as fi.
func (w *walker) child(segs []string, name, rel string, fi fs.FileInfo) error {
	childSegs := append(append(make([]string, 0, len(segs)+1), segs...), name)
	f, err := w.root.os.Open(Path{segs: childSegs}.osRel())
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed meanwhile
	}
	if err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	// If it was swapped for a symlink since it was listed, os.Root would follow it
	// (within the Upload Root, but maybe out of Scope or into a Hidden Entry): skip it.
	if opened, err := f.Stat(); err != nil || !os.SameFile(fi, opened) {
		f.Close()
		return nil
	}
	return w.dir(f, childSegs, rel)
}
