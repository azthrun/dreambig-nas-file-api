package uploadroot

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// ErrConflict means a file (or anything that is not a folder) already sits where the
// request needs a folder or a new file. It is refused, never resolved by overwriting.
var ErrConflict = errors.New("conflict")

// folderPerm is the mode of folders the service creates; the owner is the service's
// own UID/GID (the container's user).
const folderPerm = 0o755

// MkdirAll creates folder p and any missing parents for a Caller with the given Scope.
// It is idempotent: created is false when p already was a folder. Something other than
// a folder at p or at one of its parents is ErrConflict. p must pass the write-side
// path rules (see ParseEscapedDestination).
func (r *Root) MkdirAll(scope, p Path) (created bool, err error) {
	if err := p.CheckWritable(); err != nil {
		return false, err
	}
	if err := r.check(scope, p); err != nil {
		return false, err
	}
	for n := 1; n <= len(p.segs); n++ {
		rel := filepath.Join(p.segs[:n]...)
		err := r.os.Mkdir(rel, folderPerm)
		if err == nil {
			created = true
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return created, fmt.Errorf("create folder: %w", err)
		}
		fi, err := r.os.Stat(rel)
		if err != nil {
			return created, fmt.Errorf("create folder: %w", err)
		}
		if !fi.IsDir() {
			return created, ErrConflict
		}
	}
	return created, nil
}
