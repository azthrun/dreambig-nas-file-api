package uploadroot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNotFile means a path that must be a regular file is something else (a folder, a
// symlink, a device).
var ErrNotFile = errors.New("not a file")

// hashChunk is how much of a file is read at a time while hashing; memory use is
// bounded by it, whatever the file's size.
const hashChunk = 1 << 20

// Stat describes p for a Caller with the given Scope. A symlink at p is reported as
// such and not followed, as in a listing.
func (r *Root) Stat(scope, p Path) (Entry, error) {
	if _, err := r.check(scope, p); err != nil {
		return Entry{}, err
	}
	fi, err := r.os.Lstat(p.osRel())
	if err != nil {
		return Entry{}, notFound(err)
	}
	return entryOf(fi), nil
}

// SHA256 streams the regular file at p through SHA-256 and returns its hex digest with
// the Entry the digest belongs to. The digest is never cached or stored. A symlink at p
// is not followed (ErrNotFile). Reading stops early if ctx is cancelled.
func (r *Root) SHA256(ctx context.Context, scope, p Path) (Entry, string, error) {
	if _, err := r.check(scope, p); err != nil {
		return Entry{}, "", err
	}
	before, err := r.os.Lstat(p.osRel())
	if err != nil {
		return Entry{}, "", notFound(err)
	}
	if !before.Mode().IsRegular() {
		return Entry{}, "", ErrNotFile
	}
	f, err := r.os.Open(p.osRel())
	if err != nil {
		return Entry{}, "", notFound(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Entry{}, "", fmt.Errorf("stat file: %w", err)
	}
	// Replaced by a symlink or another file between the checks and the open.
	if !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		return Entry{}, "", ErrNotFile
	}
	h := sha256.New()
	buf := make([]byte, hashChunk)
	for {
		if err := ctx.Err(); err != nil {
			return Entry{}, "", fmt.Errorf("hash file: %w", err)
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Entry{}, "", fmt.Errorf("hash file: %w", err)
		}
	}
	return entryOf(fi), hex.EncodeToString(h.Sum(nil)), nil
}
