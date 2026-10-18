package server

import (
	"net/http"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/auth"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

type statJSON struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Mtime  string `json:"mtime"`
	SHA256 string `json:"sha256,omitempty"`
}

// statFile serves GET /api/v1/files/{path}: type, size and mtime of one entry, plus
// the file's SHA-256 with ?hash=sha256 (computed by streaming the file, every time).
func (h *apiHandler) statFile(w http.ResponseWriter, r *http.Request, escaped string) {
	rec := audit.FromContext(r.Context())
	rec.SetOperation("stat")
	caller, _ := auth.CallerFrom(r.Context())

	p, err := uploadroot.ParseEscapedPath(escaped)
	if err != nil {
		writePathError(w, err)
		return
	}
	rec.SetPath(p.String())

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
	)
	if withHash {
		e, sum, err = h.root.SHA256(r.Context(), caller.Scope, p)
	} else {
		e, err = h.root.Stat(caller.Scope, p)
	}
	if err != nil {
		writePathError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statJSON{
		Path:   p.String(),
		Type:   string(e.Type),
		Size:   e.Size,
		Mtime:  e.ModTime.Format(time.RFC3339Nano),
		SHA256: sum,
	})
}
