package upload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/tus/tusd/v2/pkg/filestore"
)

// Expired is an abandoned Upload that ExpireAbandoned removed.
type Expired struct {
	ID     string
	Caller string // the Caller that created it
	Path   string // its Destination Path
	Bytes  int64  // staged bytes removed
}

// ExpireAbandoned removes every incomplete Upload in the Staging Area whose staged data
// and info were both last touched more than maxAge ago, and returns what it removed. An
// Upload a request holds (e.g. one being resumed right now) is skipped, never
// interrupted. Only the Staging Area is touched; completed files are not. Failures on
// one Upload are logged and the rest still run.
func (h *Handler) ExpireAbandoned(ctx context.Context, maxAge time.Duration) ([]Expired, error) {
	dir, err := h.root.StagingDir()
	if err != nil {
		return nil, fmt.Errorf("expire uploads: %w", err)
	}
	ids, err := h.store.stagedIDs()
	if err != nil {
		return nil, fmt.Errorf("expire uploads: %w", err)
	}
	var out []Expired
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		x, removed, err := h.expireOne(ctx, dir, id, maxAge)
		if err != nil {
			h.log.ErrorContext(ctx, "expire upload", "id", id, "err", err)
			continue
		}
		if removed {
			out = append(out, x)
		}
	}
	return out, nil
}

// expireOne removes the Upload id if it is abandoned; dir is the Staging Area's OS path.
func (h *Handler) expireOne(ctx context.Context, dir, id string, maxAge time.Duration) (Expired, bool, error) {
	if stale, err := h.untouchedFor(id, maxAge); err != nil || !stale {
		return Expired{}, false, err
	}
	unlock, ok := h.locker.tryLock(id)
	if !ok {
		return Expired{}, false, nil // a request has it: in use, not abandoned
	}
	defer unlock()
	// Re-check under the lock: a request may have written to it just before.
	if stale, err := h.untouchedFor(id, maxAge); err != nil || !stale {
		return Expired{}, false, err
	}
	u, err := h.store.GetUpload(ctx, id)
	if err != nil {
		return Expired{}, false, err
	}
	info, err := u.GetInfo(ctx)
	if err != nil {
		return Expired{}, false, fmt.Errorf("read info: %w", err)
	}
	// Terminate removes the data file the info names; refuse anything but the staged
	// file itself, so a bad info can never make expiry remove a file outside the
	// Staging Area.
	if p := info.Storage[filestore.StorageKeyPath]; p != "" && p != filepath.Join(dir, id) {
		return Expired{}, false, errors.New("info names a data file outside the Staging Area")
	}
	if err := h.store.AsTerminatableUpload(u).Terminate(ctx); err != nil {
		return Expired{}, false, err
	}
	return Expired{ID: id, Caller: info.MetaData[metaCaller], Path: info.MetaData[MetaPath], Bytes: info.Offset}, true, nil
}

// untouchedFor reports whether the Upload id's staged data and info were both last
// modified more than maxAge ago. A missing data file counts as untouched.
func (h *Handler) untouchedFor(id string, maxAge time.Duration) (bool, error) {
	cutoff := time.Now().Add(-maxAge)
	for _, name := range []string{id + ".info", id} {
		fi, err := h.root.LstatStaged(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("stat staged file: %w", err)
		}
		if !fi.ModTime().Before(cutoff) {
			return false, nil
		}
	}
	return true, nil
}
