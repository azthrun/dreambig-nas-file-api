package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azthrun/dreambig-nas-file-api/internal/config"
	"github.com/azthrun/dreambig-nas-file-api/internal/server"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// Well-formed Credentials (64 hex characters), built from patterns so no
// secret-looking literal sits in the repo.
var (
	adminToken  = strings.Repeat("0123456789abcdef", 4)
	backupToken = strings.Repeat("fedcba9876543210", 4)
	wrongToken  = strings.Repeat("00112233445566778899aabbccddeeff", 2)
)

// env is a running handler over a temp Upload Root with two Callers: "admin" (whole
// Upload Root) and "backup" (Scope /backups). audit collects the audit log.
type env struct {
	t     *testing.T
	root  string
	h     http.Handler
	audit *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	root, err := uploadroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(func(k string) string {
		if k == "NAS_FILE_API_CALLERS" {
			return "admin:" + adminToken + ",backup:" + backupToken + ":/backups"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := &bytes.Buffer{}
	h, err := server.New(server.Deps{Root: root, Callers: cfg.Callers, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: dir, audit: audit, h: h}
}

func (e *env) mkdir(rel string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.root, rel), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) write(rel, content string) {
	e.t.Helper()
	e.mkdir(filepath.Dir(rel))
	if err := os.WriteFile(filepath.Join(e.root, rel), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// do sends GET target (a raw request target, sent unmodified) with the given
// Authorization header ("" for none).
func (e *env) do(target, authorization string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) as(token, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(target, "Bearer "+token)
}

func TestHealthzAnswers200WithoutCredential(t *testing.T) {
	if got := newEnv(t).do("/healthz", "").Code; got != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", got)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	if got := newEnv(t).do("/nope", "").Code; got != http.StatusNotFound {
		t.Fatalf("GET /nope = %d, want 404", got)
	}
}

func TestValidTokenIsAccepted(t *testing.T) {
	if got := newEnv(t).as(adminToken, "/api/v1/folders/").Code; got != http.StatusOK {
		t.Fatalf("GET /api/v1/folders/ with a valid token = %d, want 200", got)
	}
}

func TestMissingOrWrongCredentialIs401WithChallenge(t *testing.T) {
	cases := map[string]string{
		"missing header":  "",
		"wrong token":     "Bearer " + wrongToken,
		"short token":     "Bearer abc",
		"token prefix":    "Bearer " + adminToken[:32],
		"token plus junk": "Bearer " + adminToken + "00",
		"not bearer":      "Basic " + adminToken,
		"bearer no token": "Bearer ",
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.mkdir("photos")
			rec := e.do("/api/v1/folders/", auth)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
			if strings.Contains(rec.Body.String(), "photos") {
				t.Errorf("401 body reveals a listing: %s", rec.Body)
			}
		})
	}
}

func TestEveryAPIRouteNeedsACredential(t *testing.T) {
	e := newEnv(t)
	for _, target := range []string{"/api/v1/", "/api/v1/nope", "/api/v1/folders/x"} {
		if got := e.do(target, "").Code; got != http.StatusUnauthorized {
			t.Errorf("GET %s without Credential = %d, want 401", target, got)
		}
	}
}

// listing is the JSON body of GET /api/v1/folders/{path}.
type listing struct {
	Path    string `json:"path"`
	Entries []struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Size  int64  `json:"size"`
		Mtime string `json:"mtime"`
	} `json:"entries"`
}

func decodeListing(t *testing.T, rec *httptest.ResponseRecorder) listing {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var l listing
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return l
}

func names(l listing) []string {
	var out []string
	for _, en := range l.Entries {
		out = append(out, en.Name)
	}
	return out
}
