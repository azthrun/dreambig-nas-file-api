package server

import (
	"net/http"

	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

type statJSON struct {
	Path string `json:"path"`
	entryFields
	SHA256 string `json:"sha256,omitempty"`
}

// statFile serves GET /api/v1/files/{path}: type, size and mtime of one entry, plus
// the file's SHA-256 with ?hash=sha256 (computed by streaming the file, every time).
func (h *apiHandler) statFile(w http.ResponseWriter, r *http.Request, c call) {
	withHash := false
	if algs, ok := r.URL.Query()["hash"]; ok {
		if len(algs) != 1 || algs[0] != "sha256" {
			writeError(w, http.StatusBadRequest, "unsupported hash; use hash=sha256")
			return
		}
		withHash = true
	}

	var (
		e   uploadroot.Entry
		sum string
		err error
	)
	if withHash {
		e, sum, err = h.root.SHA256(r.Context(), c.scope, c.path)
	} else {
		e, err = h.root.Stat(c.scope, c.path)
	}
	if err != nil {
		writePathError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statJSON{Path: c.path.String(), entryFields: fieldsOf(e), SHA256: sum})
}
