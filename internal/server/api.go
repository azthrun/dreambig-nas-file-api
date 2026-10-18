package server

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/auth"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// apiHandler serves the authenticated routes.
type apiHandler struct {
	root    *uploadroot.Root
	uploads http.Handler
}

// call is one request to a path route, after the common prelude: its operation is
// audited, and its path is parsed and audited.
type call struct {
	scope uploadroot.Path // the Caller's Scope
	path  uploadroot.Path
	rec   *audit.Record
}

// endpoint is one method of a path route.
type endpoint struct {
	operation string // audit operation
	// parse validates the escaped path suffix: ParseEscapedPath for reads,
	// ParseEscapedDestination for writes.
	parse func(escaped string) (uploadroot.Path, error)
	serve func(h *apiHandler, w http.ResponseWriter, r *http.Request, c call)
}

// pathRoutes are the routes /api/v1/{name}[/{path}], by name and method.
var pathRoutes = map[string]map[string]endpoint{
	"folders": {
		http.MethodGet: {"list", uploadroot.ParseEscapedPath, (*apiHandler).listFolder},
		http.MethodPut: {"create_folder", uploadroot.ParseEscapedDestination, (*apiHandler).createFolder},
	},
	"files": {
		http.MethodGet: {"stat", uploadroot.ParseEscapedPath, (*apiHandler).statFile},
	},
	"tree": {
		http.MethodGet: {"tree", uploadroot.ParseEscapedPath, (*apiHandler).listTree},
	},
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), apiPrefix)
	name, escaped, _ := strings.Cut(rest, "/")
	if name == "uploads" {
		h.uploads.ServeHTTP(w, r) // tus routes its own methods (ADR-0003)
		return
	}
	methods, ok := pathRoutes[name]
	if !ok {
		writeError(w, http.StatusNotFound, "no such route")
		return
	}
	ep, ok := methods[r.Method]
	if !ok {
		w.Header().Set("Allow", strings.Join(slices.Sorted(maps.Keys(methods)), ", "))
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rec := audit.FromContext(r.Context())
	rec.SetOperation(ep.operation)
	caller, _ := auth.CallerFrom(r.Context())
	p, err := ep.parse(escaped)
	if err != nil {
		writePathError(w, err)
		return
	}
	rec.SetPath(p.String())
	ep.serve(h, w, r, call{scope: caller.Scope, path: p, rec: rec})
}

// entryFields are the JSON fields every listing and stat shares.
type entryFields struct {
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
}

func fieldsOf(e uploadroot.Entry) entryFields {
	return entryFields{Type: string(e.Type), Size: e.Size, Mtime: e.ModTime.Format(time.RFC3339Nano)}
}

type entryJSON struct {
	Name string `json:"name"`
	entryFields
}

// listFolder serves GET /api/v1/folders/{path}: the folder's non-hidden entries.
func (h *apiHandler) listFolder(w http.ResponseWriter, r *http.Request, c call) {
	entries, err := h.root.ReadDir(c.scope, c.path)
	if err != nil {
		writePathError(w, err)
		return
	}
	out := struct {
		Path    string      `json:"path"`
		Entries []entryJSON `json:"entries"`
	}{Path: c.path.String(), Entries: make([]entryJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, entryJSON{Name: e.Name, entryFields: fieldsOf(e)})
	}
	writeJSON(w, http.StatusOK, out)
}

// createFolder serves PUT /api/v1/folders/{path}: creates the folder and any missing
// parents. 201 when something was created, 200 when the folder already existed.
func (h *apiHandler) createFolder(w http.ResponseWriter, r *http.Request, c call) {
	created, err := h.root.MkdirAll(c.scope, c.path)
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
	}{Path: c.path.String(), Created: created})
}
