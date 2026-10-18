// Package server builds the HTTP handler for the NAS file API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/auth"
	"github.com/azthrun/dreambig-nas-file-api/internal/config"
	"github.com/azthrun/dreambig-nas-file-api/internal/upload"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// apiPrefix is where every authenticated route lives.
const apiPrefix = "/api/v1/"

// Deps are what the handler needs.
type Deps struct {
	Root    *uploadroot.Root
	Callers []config.Caller
	// Audit receives one JSON line per request other than /healthz (stdout in production).
	Audit io.Writer
	// Log receives operational errors (stderr in production); nil discards them.
	Log *slog.Logger
	// Limits restrict new Uploads; the zero value imposes none.
	Limits upload.Limits
	// UploadExpiry is how long an incomplete Upload may go untouched before
	// ExpireUploads removes it (config.Config.UploadExpiry).
	UploadExpiry time.Duration
}

// Server is the service's HTTP handler plus its background upkeep.
type Server struct {
	http.Handler
	uploads      *upload.Handler
	audit        *audit.Logger
	log          *slog.Logger
	uploadExpiry time.Duration
}

// uploadsPath is where the tus endpoint for Uploads lives (ADR-0003).
const uploadsPath = apiPrefix + "uploads/"

// New returns the service's HTTP handler.
//
// Requests under /api/v1/ are routed without http.ServeMux, whose path cleaning would
// silently redirect "..", "." and "//" instead of letting the path validator refuse
// them.
func New(d Deps) (*Server, error) {
	if d.UploadExpiry <= 0 {
		return nil, errors.New("UploadExpiry must be positive")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	uploads, err := upload.New(d.Root, uploadsPath, d.Limits, log)
	if err != nil {
		return nil, fmt.Errorf("uploads: %w", err)
	}
	auditLog := audit.New(d.Audit)
	api := auth.New(d.Callers).Middleware(&apiHandler{root: d.Root, uploads: uploads})
	audited := auditLog.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiPrefix) || r.URL.Path == strings.TrimSuffix(apiPrefix, "/") {
			api.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness only; no Credential needed, reveals nothing about the Upload Root.
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		audited.ServeHTTP(w, r)
	})
	return &Server{Handler: h, uploads: uploads, audit: auditLog, log: log, uploadExpiry: d.UploadExpiry}, nil
}

// ExpireUploads removes incomplete Uploads untouched for longer than UploadExpiry,
// logging one audit line per removal.
func (s *Server) ExpireUploads(ctx context.Context) error {
	expired, err := s.uploads.ExpireAbandoned(ctx, s.uploadExpiry)
	for _, x := range expired {
		s.audit.Event(x.Caller, "upload_expire", x.Path, "expired", x.Bytes)
	}
	return err
}

// RunUploadExpiry runs ExpireUploads now and then every interval until ctx is done.
func (s *Server) RunUploadExpiry(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.ExpireUploads(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "expire uploads", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// writePathError answers an uploadroot error. Hidden Entries look like missing ones,
// and no message contains an absolute NAS path.
func writePathError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, uploadroot.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, uploadroot.ErrOutOfScope):
		writeError(w, http.StatusForbidden, "outside your Scope")
	case errors.Is(err, uploadroot.ErrHidden), errors.Is(err, uploadroot.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, uploadroot.ErrNotFolder):
		writeError(w, http.StatusBadRequest, "not a folder")
	case errors.Is(err, uploadroot.ErrNotFile):
		writeError(w, http.StatusBadRequest, "not a file")
	case errors.Is(err, uploadroot.ErrConflict):
		writeError(w, http.StatusConflict, "a file is in the way")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
