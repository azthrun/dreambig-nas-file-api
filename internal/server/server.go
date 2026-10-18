// Package server builds the HTTP handler for the NAS file API.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/auth"
	"github.com/azthrun/dreambig-nas-file-api/internal/config"
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
}

// New returns the service's HTTP handler.
//
// Requests under /api/v1/ are routed without http.ServeMux, whose path cleaning would
// silently redirect "..", "." and "//" instead of letting the path validator refuse
// them.
func New(d Deps) http.Handler {
	api := auth.New(d.Callers).Middleware(&apiHandler{root: d.Root})
	audited := audit.New(d.Audit).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiPrefix) || r.URL.Path == strings.TrimSuffix(apiPrefix, "/") {
			api.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness only; no Credential needed, reveals nothing about the Upload Root.
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		audited.ServeHTTP(w, r)
	})
}

// apiHandler serves the authenticated routes.
type apiHandler struct {
	root *uploadroot.Root
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), strings.TrimSuffix(apiPrefix, "/"))
	switch {
	case rest == "/folders" || strings.HasPrefix(rest, "/folders/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.listFolder(w, r, strings.TrimPrefix(strings.TrimPrefix(rest, "/folders"), "/"))
	case rest == "/files" || strings.HasPrefix(rest, "/files/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.statFile(w, r, strings.TrimPrefix(strings.TrimPrefix(rest, "/files"), "/"))
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
