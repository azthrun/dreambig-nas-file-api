package upload

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"

	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// filePerm is the mode of staged files, and so of completed ones (they are the same
// inode).
const filePerm = 0o644

// errInternal is what a Caller sees for any unexpected failure: tusd would otherwise
// echo the error text, which may contain an absolute NAS path.
var errInternal = handler.NewError("ERR_INTERNAL_SERVER_ERROR", "internal error", 500)

// store is the Staging Area as a tusd data store: tusd's filestore in the Upload Root's
// Staging Area, created on first use, with errors scrubbed of NAS paths.
//
// It is the seam for follow-ups: expiring abandoned Uploads (#9) lists and terminates
// what is here; a free-space reserve or per-Caller limit (#10) belongs in NewUpload and
// WriteChunk.
type store struct {
	root *uploadroot.Root
	log  *slog.Logger
}

// files returns the filestore over the Staging Area, creating the folder if needed.
func (s *store) files() (filestore.FileStore, error) {
	dir, err := s.root.StagingDir()
	if err != nil {
		return filestore.FileStore{}, err
	}
	fs := filestore.New(dir)
	fs.DirModePerm, fs.FileModePerm = 0o700, filePerm
	return fs, nil
}

func (s *store) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	fs, err := s.files()
	if err != nil {
		return nil, s.scrub(ctx, "staging: new upload", err)
	}
	u, err := fs.NewUpload(ctx, info)
	if err != nil {
		return nil, s.scrub(ctx, "staging: new upload", err)
	}
	return &stagedUpload{Upload: u, fs: fs, s: s}, nil
}

func (s *store) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	fs, err := s.files()
	if err != nil {
		return nil, s.scrub(ctx, "staging: get upload", err)
	}
	u, err := fs.GetUpload(ctx, id)
	if err != nil {
		return nil, s.scrub(ctx, "staging: get upload", err)
	}
	return &stagedUpload{Upload: u, fs: fs, s: s}, nil
}

func (s *store) AsTerminatableUpload(u handler.Upload) handler.TerminatableUpload {
	return u.(*stagedUpload)
}

// scrub passes tusd's own errors (they carry a status and a safe message) and replaces
// anything else with errInternal, logging the real error.
func (s *store) scrub(ctx context.Context, what string, err error) error {
	var tusErr handler.Error
	if errors.As(err, &tusErr) {
		return err
	}
	s.log.ErrorContext(ctx, what, "err", err)
	return errInternal
}

// stagedUpload is one Upload's data in the Staging Area.
type stagedUpload struct {
	handler.Upload
	fs filestore.FileStore
	s  *store
}

func (u *stagedUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	n, err := u.Upload.WriteChunk(ctx, offset, src)
	if err != nil {
		err = u.s.scrub(ctx, "staging: write", err)
	}
	return n, err
}

// Terminate removes the Upload's staged data and info: the only removal the service
// ever does (CODING_STANDARDS.md).
func (u *stagedUpload) Terminate(ctx context.Context) error {
	if err := u.fs.AsTerminatableUpload(u.Upload).Terminate(ctx); err != nil {
		return u.s.scrub(ctx, "staging: terminate", err)
	}
	return nil
}
