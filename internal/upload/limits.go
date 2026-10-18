package upload

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
)

// Limits keep Uploads from filling the NAS or overwhelming it (#10). The zero value
// imposes no limits.
//
// An active Upload is one whose data is in the Staging Area and has not all arrived: it
// was created and is not yet completed, cancelled, discarded (Conflict, hash mismatch)
// or expired. Because this is read from the Staging Area, active Uploads keep counting
// across restarts.
type Limits struct {
	// MaxActivePerCaller is how many active Uploads one Caller may have; creating
	// another is refused with 429 until one completes or is cancelled. 0 means no limit.
	MaxActivePerCaller int
	// FreeSpaceReserve is the free space, in bytes, that must remain on the Upload
	// Root's volume once every active Upload (and the new one) has all its bytes.
	FreeSpaceReserve int64
	// FreeSpace reports the bytes available on the Upload Root's volume. nil means ask
	// the filesystem; tests substitute a fake.
	FreeSpace func() (int64, error)
}

// retryAfterSeconds is what a Caller over its Upload limit is told to wait.
const retryAfterSeconds = "30"

// statusInsufficientStorage is WebDAV's 507, for a volume that cannot take the Upload.
const statusInsufficientStorage = http.StatusInsufficientStorage

func errTooManyUploads(max int) error {
	err := handler.NewError("ERR_TOO_MANY_UPLOADS",
		fmt.Sprintf("you already have %d active Uploads, the most allowed; retry once one completes or is cancelled", max),
		http.StatusTooManyRequests)
	err.HTTPResponse.Header["Retry-After"] = retryAfterSeconds
	return err
}

var errInsufficientSpace = handler.NewError("ERR_INSUFFICIENT_SPACE",
	"the NAS does not have room for this Upload while keeping its free-space reserve",
	statusInsufficientStorage)

// admit refuses a new Upload that would break the Limits. The caller holds s.admitMu
// until the Upload is created, so concurrent creates cannot both squeeze in.
func (s *store) admit(ctx context.Context, fs filestore.FileStore, info handler.FileInfo) error {
	lim := s.limits
	if lim.MaxActivePerCaller <= 0 && lim.FreeSpaceReserve <= 0 && lim.FreeSpace == nil {
		return nil
	}
	active, err := s.active(ctx, fs)
	if err != nil {
		return s.scrub(ctx, "staging: list active uploads", err)
	}
	rec := audit.FromContext(ctx)

	if lim.MaxActivePerCaller > 0 {
		caller, n := info.MetaData[metaCaller], 0
		for _, a := range active {
			if a.MetaData[metaCaller] == caller {
				n++
			}
		}
		if n >= lim.MaxActivePerCaller {
			rec.SetOutcome("too_many_uploads")
			return errTooManyUploads(lim.MaxActivePerCaller)
		}
	}

	free, err := s.freeSpace()
	if err != nil {
		return s.scrub(ctx, "free space", err)
	}
	pending := info.Size
	for _, a := range active {
		pending += a.Size - a.Offset
	}
	if free-pending < lim.FreeSpaceReserve {
		rec.SetOutcome("insufficient_space")
		return errInsufficientSpace
	}
	return nil
}

func (s *store) freeSpace() (int64, error) {
	if s.limits.FreeSpace != nil {
		return s.limits.FreeSpace()
	}
	return s.root.FreeSpace()
}

// active lists the active Uploads in the Staging Area.
func (s *store) active(ctx context.Context, fs filestore.FileStore) ([]handler.FileInfo, error) {
	des, err := os.ReadDir(fs.Path)
	if err != nil {
		return nil, fmt.Errorf("read Staging Area: %w", err)
	}
	var out []handler.FileInfo
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".info")
		if !ok || !idRe.MatchString(id) {
			continue
		}
		u, err := fs.GetUpload(ctx, id)
		if errors.Is(err, handler.ErrNotFound) {
			continue // completed or cancelled meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("staged upload %s: %w", id, err)
		}
		info, err := u.GetInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("staged upload %s: %w", id, err)
		}
		// All bytes in but still staged: completing, or completed with a staged name
		// left for expiry. Either way it no longer occupies a slot or needs space.
		if info.Offset >= info.Size {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}
