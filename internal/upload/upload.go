// Package upload serves resumable Uploads over tus through embedded tusd (ADR-0003).
// Each Upload is one file to one Destination Path. Its data waits in the Staging Area
// until the last byte arrives; then it is checked for a Conflict, hashed, verified
// against a declared SHA-256 and hard-linked into place, never overwriting anything.
//
// tus Upload-Metadata keys:
//
//	path    Destination Path, relative to the Upload Root ("photos/a.jpg" or "/photos/a.jpg"); required
//	mtime   the file's modification time, RFC 3339 (e.g. 2021-03-04T05:06:07Z); required
//	sha256  the whole file's SHA-256, hex; optional
//
// The server's SHA-256 of the completed file is returned in the Upload-Sha256 header of
// the response that completes the Upload.
package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
	expslog "golang.org/x/exp/slog"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/auth"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// Upload-Metadata keys.
const (
	MetaPath   = "path"
	MetaMtime  = "mtime"
	MetaSHA256 = "sha256"
	// metaCaller records the Caller that created the Upload; only it may touch it.
	metaCaller = "caller"
)

// HeaderSHA256 carries the server's SHA-256 of a completed Upload.
const HeaderSHA256 = "Upload-Sha256"

// statusChecksumMismatch is tus's status for a checksum mismatch.
const statusChecksumMismatch = 460

// idRe matches the IDs tusd's filestore generates (128 random bits, hex).
var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

var (
	errConflict     = handler.NewError("ERR_CONFLICT", "a file already exists at the Destination Path, or a file stands in place of one of its folders", http.StatusConflict)
	errOutOfScope   = handler.NewError("ERR_OUT_OF_SCOPE", "outside your Scope", http.StatusForbidden)
	errHashMismatch = handler.NewError("ERR_CHECKSUM_MISMATCH", "the file's SHA-256 does not match the declared sha256; the Upload was discarded", statusChecksumMismatch)
	errNotFound     = handler.NewError("ERR_UPLOAD_NOT_FOUND", "upload not found", http.StatusNotFound)
	// errHiddenDestination answers a Destination Path that reaches a Hidden Entry through
	// a symlink: like the listings, it looks missing.
	errHiddenDestination = handler.NewError("ERR_NOT_FOUND", "not found", http.StatusNotFound)
)

// Handler serves the tus endpoint. It must sit behind auth.Middleware and
// audit.Middleware.
type Handler struct {
	root     *uploadroot.Root
	basePath string
	store    *store
	locker   *locker
	tus      *handler.UnroutedHandler
	routed   http.Handler
	log      *slog.Logger
}

// New returns a Handler for Uploads into root, served under basePath (e.g.
// "/api/v1/uploads/"), refusing new Uploads that would break limits. log receives
// operational errors; nil discards them.
func New(root *uploadroot.Root, basePath string, limits Limits, log *slog.Logger) (*Handler, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &Handler{root: root, basePath: basePath, log: log, store: &store{root: root, log: log, limits: limits}, locker: newLocker()}

	composer := handler.NewStoreComposer()
	composer.UseCore(h.store)
	composer.UseTerminater(h.store)
	composer.UseLocker(h.locker)

	cors := handler.DefaultCorsConfig
	cors.ExposeHeaders += ", " + HeaderSHA256
	tus, err := handler.NewUnroutedHandler(handler.Config{
		StoreComposer:             composer,
		BasePath:                  basePath,
		DisableDownload:           true, // ADR-0001: nothing is read back
		DisableConcatenation:      true,
		Cors:                      &cors,
		RespectForwardedHeaders:   true, // behind `tailscale serve`, which sets them (ADR-0002)
		PreUploadCreateCallback:   h.preCreate,
		PreFinishResponseCallback: h.preFinish,
		// tusd logs every request at info level; our audit log covers that.
		Logger: expslog.New(expslog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return nil, fmt.Errorf("tusd: %w", err)
	}
	h.tus = tus
	h.routed = tus.Middleware(http.HandlerFunc(h.route))
	return h, nil
}

// ServeHTTP serves basePath (create) and basePath/{id} (status, resume, cancel).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, h.basePath)
	if r.URL.Path == strings.TrimSuffix(h.basePath, "/") {
		id = ""
	}
	if id != "" && !idRe.MatchString(id) {
		writeTusError(w, errNotFound)
		return
	}
	// tusd takes the Upload ID to be the whole request path.
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = "/"+id, ""
	body := &countingReader{r: r.Body}
	if r.Body != nil {
		r2.Body = body
	}
	h.routed.ServeHTTP(w, r2)
	if body.n > 0 {
		audit.FromContext(r.Context()).SetBytes(body.n)
	}
}

// route runs after tusd's middleware (which may apply X-HTTP-Method-Override).
func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	rec := audit.FromContext(r.Context())
	id := strings.TrimPrefix(r.URL.Path, "/")
	if id == "" {
		rec.SetOperation("upload_create")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost+", "+http.MethodOptions)
			writeTusError(w, handler.NewError("ERR_METHOD_NOT_ALLOWED", "method not allowed", http.StatusMethodNotAllowed))
			return
		}
		h.tus.PostFile(w, r)
		return
	}

	var serve http.HandlerFunc
	switch r.Method {
	case http.MethodHead:
		rec.SetOperation("upload_status")
		serve = h.tus.HeadFile
	case http.MethodPatch:
		rec.SetOperation("upload")
		serve = h.tus.PatchFile
	case http.MethodDelete:
		rec.SetOperation("upload_cancel")
		serve = h.tus.DelFile
	default:
		w.Header().Set("Allow", "HEAD, PATCH, DELETE, OPTIONS")
		writeTusError(w, handler.NewError("ERR_METHOD_NOT_ALLOWED", "method not allowed", http.StatusMethodNotAllowed))
		return
	}
	// Only the Caller that created an Upload may see or touch it; to anyone else it
	// does not exist.
	caller, _ := auth.CallerFrom(r.Context())
	u, err := h.store.GetUpload(r.Context(), id)
	if err != nil {
		writeTusError(w, err)
		return
	}
	info, err := u.GetInfo(r.Context())
	if err != nil || info.MetaData[metaCaller] != caller.Name {
		writeTusError(w, errNotFound)
		return
	}
	rec.SetPath(info.MetaData[MetaPath])
	serve(w, r)
}

// destination is a validated Upload's metadata.
type destination struct {
	path   uploadroot.Path
	mtime  time.Time
	sha256 string // lower-case hex, or "" when not declared
}

func parseMeta(m handler.MetaData) (destination, error) {
	var d destination
	raw, ok := m[MetaPath]
	if !ok || raw == "" {
		return d, badRequest("ERR_MISSING_PATH", "Upload-Metadata needs path (the Destination Path)")
	}
	p, err := uploadroot.ParseDestination(strings.TrimPrefix(raw, "/"))
	if err != nil {
		return d, badRequest("ERR_INVALID_PATH", err.Error())
	}
	d.path = p
	rawMtime, ok := m[MetaMtime]
	if !ok || rawMtime == "" {
		return d, badRequest("ERR_MISSING_MTIME", "Upload-Metadata needs mtime (RFC 3339)")
	}
	if d.mtime, err = time.Parse(time.RFC3339Nano, rawMtime); err != nil {
		return d, badRequest("ERR_INVALID_MTIME", "mtime must be RFC 3339, e.g. 2021-03-04T05:06:07Z")
	}
	if s, ok := m[MetaSHA256]; ok && s != "" {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != sha256.Size {
			return d, badRequest("ERR_INVALID_SHA256", "sha256 must be 64 hex characters")
		}
		d.sha256 = hex.EncodeToString(b)
	}
	return d, nil
}

// preCreate validates a new Upload: metadata, path rules, Scope and Conflict. It
// stores the normalized Destination Path and the Caller in the Upload's metadata.
func (h *Handler) preCreate(ev handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	none := handler.FileInfoChanges{}
	rec := audit.FromContext(ev.Context)
	rec.SetOperation("upload_create")
	caller, ok := auth.CallerFrom(ev.Context)
	if !ok {
		return handler.HTTPResponse{}, none, errInternal
	}
	// A refusal names the Destination Path as sent until it is parsed, never the URL.
	rec.SetPath(ev.Upload.MetaData[MetaPath])
	d, err := parseMeta(ev.Upload.MetaData)
	if err != nil {
		return handler.HTTPResponse{}, none, err
	}
	rec.SetPath(d.path.String())
	if err := h.root.CheckNewFile(caller.Scope, d.path); err != nil {
		return handler.HTTPResponse{}, none, h.pathError(ev.Context, err)
	}
	m := handler.MetaData{}
	for k, v := range ev.Upload.MetaData {
		m[k] = v
	}
	m[MetaPath] = d.path.String()
	m[metaCaller] = caller.Name
	if d.sha256 != "" {
		m[MetaSHA256] = d.sha256
	}
	return handler.HTTPResponse{}, handler.FileInfoChanges{MetaData: m}, nil
}

// preFinish completes an Upload whose last byte has arrived: re-check for a Conflict,
// hash, verify, link into place with its mtime, and return the hash. On any failure
// before the file is in place the staged data is discarded, so a retry cannot find a
// "complete" Upload that never landed.
func (h *Handler) preFinish(ev handler.HookEvent) (handler.HTTPResponse, error) {
	ctx := ev.Context
	rec := audit.FromContext(ctx)
	caller, _ := auth.CallerFrom(ctx)
	u, err := h.store.GetUpload(ctx, ev.Upload.ID)
	if err != nil {
		return handler.HTTPResponse{}, err
	}
	discard := func(cause error) (handler.HTTPResponse, error) {
		if err := h.store.AsTerminatableUpload(u).Terminate(ctx); err != nil {
			h.log.ErrorContext(ctx, "discard staged upload", "id", ev.Upload.ID, "err", err)
		}
		return handler.HTTPResponse{}, cause
	}

	d, err := parseMeta(ev.Upload.MetaData)
	if err != nil {
		return discard(err)
	}
	rec.SetPath(d.path.String())
	if err := h.root.CheckNewFile(caller.Scope, d.path); err != nil {
		return discard(h.pathError(ctx, err))
	}
	sum, err := hashUpload(ctx, u)
	if err != nil {
		h.log.ErrorContext(ctx, "hash staged upload", "id", ev.Upload.ID, "err", err)
		return discard(errInternal)
	}
	if d.sha256 != "" && d.sha256 != sum {
		return discard(errHashMismatch)
	}
	if err := h.root.PlaceStaged(caller.Scope, d.path, ev.Upload.ID, d.mtime); err != nil {
		return discard(h.pathError(ctx, err))
	}
	// The file is in place; failing to drop the staged name only leaves a link for
	// expiry (#9) to remove.
	if err := h.store.AsTerminatableUpload(u).Terminate(ctx); err != nil {
		h.log.ErrorContext(ctx, "remove staged upload after completion", "id", ev.Upload.ID, "err", err)
	}
	rec.SetOutcome("completed")
	return handler.HTTPResponse{Header: handler.HTTPHeader{HeaderSHA256: sum}}, nil
}

func hashUpload(ctx context.Context, u handler.Upload) (string, error) {
	r, err := u.GetReader(ctx)
	if err != nil {
		return "", err
	}
	defer r.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// pathError maps an uploadroot error to a tus error without NAS paths.
func (h *Handler) pathError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, uploadroot.ErrInvalidPath):
		return badRequest("ERR_INVALID_PATH", err.Error())
	case errors.Is(err, uploadroot.ErrOutOfScope):
		return errOutOfScope
	case errors.Is(err, uploadroot.ErrConflict):
		return errConflict
	case errors.Is(err, uploadroot.ErrHidden):
		return errHiddenDestination
	default:
		h.log.ErrorContext(ctx, "upload path", "err", err)
		return errInternal
	}
}

func badRequest(code, msg string) error {
	return handler.NewError(code, msg, http.StatusBadRequest)
}

func writeTusError(w http.ResponseWriter, err error) {
	var tusErr handler.Error
	if !errors.As(err, &tusErr) {
		tusErr = errInternal
	}
	for k, v := range tusErr.HTTPResponse.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.WriteHeader(tusErr.HTTPResponse.StatusCode)
	_, _ = io.WriteString(w, tusErr.HTTPResponse.Body)
}

type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }
