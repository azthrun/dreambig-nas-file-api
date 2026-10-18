package server

import (
	"encoding/json"
	"net/http"

	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

type treeEntryJSON struct {
	Path string `json:"path"`
	entryFields
}

// listTree serves GET /api/v1/tree/{path}: every non-hidden entry below the folder, one
// JSON line each (application/x-ndjson), streamed as the folder is walked. Paths are
// relative to the requested folder.
func (h *apiHandler) listTree(w http.ResponseWriter, r *http.Request, c call) {
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
	err := h.root.Walk(r.Context(), c.scope, c.path, func(rel string, e uploadroot.Entry) error {
		start()
		writeErr = enc.Encode(treeEntryJSON{Path: rel, entryFields: fieldsOf(e)})
		return writeErr
	})
	switch {
	case err == nil:
		start()
	case writeErr != nil || r.Context().Err() != nil:
		// The client went away; nobody is left to tell.
		c.rec.SetOutcome("aborted")
	case !started:
		writePathError(w, err)
	default:
		// The status is already sent: end the stream with an error line, so the client
		// knows the listing is incomplete.
		c.rec.SetOutcome("error")
		_ = enc.Encode(map[string]string{"error": "listing failed partway; it is incomplete"})
	}
}
