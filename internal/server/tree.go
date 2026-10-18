package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/auth"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

type treeEntryJSON struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
}

// listTree serves GET /api/v1/tree/{path}: every non-hidden entry below the folder, one
// JSON line each (application/x-ndjson), streamed as the folder is walked. Paths are
// relative to the requested folder.
func (h *apiHandler) listTree(w http.ResponseWriter, r *http.Request, escaped string) {
	rec := audit.FromContext(r.Context())
	rec.SetOperation("tree")
	caller, _ := auth.CallerFrom(r.Context())

	p, err := uploadroot.ParseEscapedPath(escaped)
	if err != nil {
		writePathError(w, err)
		return
	}
	rec.SetPath(p.String())

	enc := json.NewEncoder(w)
	started := false
	start := func() {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			started = true
		}
	}
	var writeErr error
	err = h.root.Walk(r.Context(), caller.Scope, p, func(rel string, e uploadroot.Entry) error {
		start()
		writeErr = enc.Encode(treeEntryJSON{
			Path:  rel,
			Type:  string(e.Type),
			Size:  e.Size,
			Mtime: e.ModTime.Format(time.RFC3339Nano),
		})
		return writeErr
	})
	switch {
	case err == nil:
		start()
	case writeErr != nil || r.Context().Err() != nil:
		// The client went away; nobody is left to tell.
		rec.SetOutcome("aborted")
	case !started:
		writePathError(w, err)
	default:
		// The status is already sent: end the stream with an error line, so the client
		// knows the listing is incomplete.
		rec.SetOutcome("error")
		_ = enc.Encode(map[string]string{"error": "listing failed partway; it is incomplete"})
	}
}
