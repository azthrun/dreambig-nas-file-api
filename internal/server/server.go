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
	uploads, err := upload.New(d.Root, uploadsPath, log)
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

// apiHandler serves the authenticated routes.
type apiHandler struct {
	root    *uploadroot.Root
	uploads http.Handler
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), strings.TrimSuffix(apiPrefix, "/"))
	switch {
	case rest == "/folders" || strings.HasPrefix(rest, "/folders/"):
		escaped := strings.TrimPrefix(strings.TrimPrefix(rest, "/folders"), "/")
		switch r.Method {
		case http.MethodGet:
			h.listFolder(w, r, escaped)
		case http.MethodPut:
			h.createFolder(w, r, escaped)
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case rest == "/uploads" || strings.HasPrefix(rest, "/uploads/"):
		h.uploads.ServeHTTP(w, r)
	case rest == "/files" || strings.HasPrefix(rest, "/files/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.statFile(w, r, strings.TrimPrefix(strings.TrimPrefix(rest, "/files"), "/"))
	case rest == "/tree" || strings.HasPrefix(rest, "/tree/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.listTree(w, r, strings.TrimPrefix(strings.TrimPrefix(rest, "/tree"), "/"))
	default:
		writeError(w, http.StatusNotFound, "no such route")
	}
}

type entryJSON struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
}

// listFolder serves GET /api/v1/folders/{path}: the folder's non-hidden entries.
func (h *apiHandler) listFolder(w http.ResponseWriter, r *http.Request, escaped string) {
	rec := audit.FromContext(r.Context())
	rec.SetOperation("list")
	caller, _ := auth.CallerFrom(r.Context())

	p, err := uploadroot.ParseEscapedPath(escaped)
	if err != nil {
		writePathError(w, err)
		return
	}
	rec.SetPath(p.String())
	entries, err := h.root.ReadDir(caller.Scope, p)
	if err != nil {
		writePathError(w, err)
		return
	}
	out := struct {
		Path    string      `json:"path"`
		Entries []entryJSON `json:"entries"`
	}{Path: p.String(), Entries: make([]entryJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, entryJSON{
			Name:  e.Name,
			Type:  string(e.Type),
			Size:  e.Size,
			Mtime: e.ModTime.Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// createFolder serves PUT /api/v1/folders/{path}: creates the folder and any missing
// parents. 201 when something was created, 200 when the folder already existed.
func (h *apiHandler) createFolder(w http.ResponseWriter, r *http.Request, escaped string) {
	rec := audit.FromContext(r.Context())
	rec.SetOperation("create_folder")
	caller, _ := auth.CallerFrom(r.Context())
	p, err := uploadroot.ParseEscapedDestination(escaped)
	if err != nil {
		writePathError(w, err)
		return
	}
	rec.SetPath(p.String())
	created, err := h.root.MkdirAll(caller.Scope, p)
	if err != nil {
		writePathError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, struct {
		Path    string `json:"path"`
		Created bool   `json:"created"`
	}{Path: p.String(), Created: created})
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
