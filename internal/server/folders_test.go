package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestListFolderReturnsNameTypeSizeAndMtime(t *testing.T) {
	e := newEnv(t)
	e.write("photos/2024/a.jpg", "12345")
	e.mkdir("photos/2024/trip")
	mtime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(e.root, "photos/2024/a.jpg"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	l := decodeListing(t, e.as(adminToken, "/api/v1/folders/photos/2024"))
	if l.Path != "/photos/2024" {
		t.Errorf("path = %q, want /photos/2024", l.Path)
	}
	if len(l.Entries) != 2 {
		t.Fatalf("entries = %+v, want a.jpg and trip", l.Entries)
	}
	file, folder := l.Entries[0], l.Entries[1]
	if file.Name != "a.jpg" || file.Type != "file" || file.Size != 5 || file.Mtime != "2024-06-01T12:00:00Z" {
		t.Errorf("file entry = %+v", file)
	}
	if folder.Name != "trip" || folder.Type != "folder" {
		t.Errorf("folder entry = %+v", folder)
	}
}

func TestListUploadRootAndEmptyFolder(t *testing.T) {
	e := newEnv(t)
	e.mkdir("empty")
	if got := names(decodeListing(t, e.as(adminToken, "/api/v1/folders/"))); !slices.Equal(got, []string{"empty"}) {
		t.Errorf("root entries = %v, want [empty]", got)
	}
	rec := e.as(adminToken, "/api/v1/folders/empty")
	if l := decodeListing(t, rec); l.Entries == nil || len(l.Entries) != 0 {
		t.Errorf("empty folder body = %s, want an empty entries array", rec.Body)
	}
}

func TestListMissingFolderIs404AndFileIs400(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "x")
	if got := e.as(adminToken, "/api/v1/folders/nope").Code; got != http.StatusNotFound {
		t.Errorf("missing folder = %d, want 404", got)
	}
	if got := e.as(adminToken, "/api/v1/folders/a.txt").Code; got != http.StatusBadRequest {
		t.Errorf("listing a file = %d, want 400", got)
	}
}

func TestListingIsOnlyGET(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/folders/x", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, PUT" {
		t.Errorf("Allow = %q, want %q", got, "GET, PUT")
	}
}

func TestScopedCallerListsOnlyInsideScope(t *testing.T) {
	e := newEnv(t)
	e.write("backups/x/db.dump", "data")
	e.mkdir("photos")
	e.mkdir("backups-old")

	if got := names(decodeListing(t, e.as(backupToken, "/api/v1/folders/backups/x"))); !slices.Equal(got, []string{"db.dump"}) {
		t.Errorf("/backups/x = %v, want [db.dump]", got)
	}
	if got := e.as(backupToken, "/api/v1/folders/backups").Code; got != http.StatusOK {
		t.Errorf("/backups (the Scope itself) = %d, want 200", got)
	}
	for _, target := range []string{
		"/api/v1/folders/",
		"/api/v1/folders",
		"/api/v1/folders/photos",
		"/api/v1/folders/backups-old", // shares a prefix, but is not inside /backups
		"/api/v1/folders/Backups/x",   // case-sensitive, even on a case-insensitive disk
		"/api/v1/folders/nope",        // refused before revealing whether it exists
	} {
		if got := e.as(backupToken, target).Code; got != http.StatusForbidden {
			t.Errorf("Scope /backups: GET %s = %d, want 403", target, got)
		}
	}
}

func TestHiddenEntriesNeverAppearInAListing(t *testing.T) {
	e := newEnv(t)
	e.mkdir("@eaDir")
	e.mkdir("#recycle")
	e.mkdir(".nas-file-api-staging")
	e.mkdir("photos/@eaDir")
	e.write("photos/#snapshot", "x")
	e.write("photos/a.jpg", "x")
	e.mkdir("photos/.nas-file-api-staging") // only the Upload Root's one is the Staging Area

	if got := names(decodeListing(t, e.as(adminToken, "/api/v1/folders/"))); !slices.Equal(got, []string{"photos"}) {
		t.Errorf("root entries = %v, want [photos]", got)
	}
	if got := names(decodeListing(t, e.as(adminToken, "/api/v1/folders/photos"))); !slices.Equal(got, []string{".nas-file-api-staging", "a.jpg"}) {
		t.Errorf("photos entries = %v", got)
	}
}

func TestHiddenEntriesCannotBeListedDirectly(t *testing.T) {
	e := newEnv(t)
	e.mkdir("@eaDir")
	e.mkdir("#recycle")
	e.mkdir(".nas-file-api-staging")
	e.mkdir("photos/@eaDir")
	for _, target := range []string{
		"/api/v1/folders/@eaDir",
		"/api/v1/folders/%23recycle",
		"/api/v1/folders/.nas-file-api-staging",
		"/api/v1/folders/photos/@eaDir",
	} {
		if got := e.as(adminToken, target).Code; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, got)
		}
	}
}

func TestPathValidationRules(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a/b.jpg", "x")
	cases := map[string]string{
		"dot-dot":                 "/api/v1/folders/photos/../photos",
		"encoded dot-dot":         "/api/v1/folders/photos/%2e%2e/photos",
		"leading dot-dot":         "/api/v1/folders/..",
		"dot":                     "/api/v1/folders/photos/./a",
		"absolute":                "/api/v1/folders//etc",
		"empty segment":           "/api/v1/folders/photos//a",
		"trailing slash":          "/api/v1/folders/photos/",
		"encoded slash":           "/api/v1/folders/photos%2Fa",
		"backslash":               "/api/v1/folders/photos%5Ca",
		"control character":       "/api/v1/folders/photos%0Aa",
		"NUL":                     "/api/v1/folders/photos%00",
		"invalid UTF-8":           "/api/v1/folders/%FF",
		"dot-dot after real path": "/api/v1/folders/photos/a/../../..",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			rec := e.as(adminToken, target)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %s = %d, want 400; body %s", target, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), e.root) {
				t.Errorf("error body leaks the Upload Root's absolute path: %s", rec.Body)
			}
		})
	}
}

func TestPathsAreNFCNormalized(t *testing.T) {
	e := newEnv(t)
	e.mkdir("café") // NFC, as the service would create it
	e.write("café/menu.txt", "x")
	// A macOS client sends NFD: "e" + combining acute accent.
	l := decodeListing(t, e.as(adminToken, "/api/v1/folders/cafe%CC%81"))
	if l.Path != "/café" {
		t.Errorf("path = %q, want NFC /café", l.Path)
	}
}

func TestSymlinkEscapingTheUploadRootIsRefused(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mkdir("photos")
	if err := os.Symlink(outside, filepath.Join(e.root, "photos", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../..", filepath.Join(e.root, "photos", "up")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/v1/folders/photos/escape", "/api/v1/folders/photos/up"} {
		rec := e.as(adminToken, target)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret.txt") || strings.Contains(rec.Body.String(), outside) {
			t.Errorf("GET %s leaks the outside folder: %s", target, rec.Body)
		}
	}
	// Listed as a symlink, never followed.
	l := decodeListing(t, e.as(adminToken, "/api/v1/folders/photos"))
	for _, en := range l.Entries {
		if en.Type != "symlink" {
			t.Errorf("entry %+v, want type symlink", en)
		}
	}
}

func TestSymlinkOutOfScopeIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mkdir("photos")
	e.mkdir("backups")
	if err := os.Symlink("../photos", filepath.Join(e.root, "backups", "pics")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../@eaDir", filepath.Join(e.root, "backups", "meta")); err != nil {
		t.Fatal(err)
	}
	e.mkdir("@eaDir")
	if got := e.as(backupToken, "/api/v1/folders/backups/pics").Code; got != http.StatusForbidden {
		t.Errorf("symlink from Scope to /photos = %d, want 403", got)
	}
	// Like naming the Hidden Entry directly (see TestHiddenEntriesReachedThroughASymlinkAre404).
	if got := e.as(adminToken, "/api/v1/folders/backups/meta").Code; got != http.StatusNotFound {
		t.Errorf("symlink to a Hidden Entry = %d, want 404", got)
	}
}

// auditLines decodes the audit log written so far.
func auditLines(t *testing.T, e *env) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(e.audit.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit line %q is not JSON: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestAuditLogsOneLinePerRequestWithoutTokens(t *testing.T) {
	e := newEnv(t)
	e.write("backups/x/db.dump", "data")
	e.as(backupToken, "/api/v1/folders/backups/x")
	e.as(backupToken, "/api/v1/folders/photos")
	e.as(wrongToken, "/api/v1/folders/backups")
	e.do("/api/v1/folders/backups", "")
	e.do("/healthz", "")

	lines := auditLines(t, e)
	if len(lines) != 4 {
		t.Fatalf("got %d audit lines, want 4 (one per API request): %s", len(lines), e.audit)
	}
	ok, refused, wrong, missing := lines[0], lines[1], lines[2], lines[3]

	for _, field := range []string{"time", "caller", "operation", "path", "outcome", "bytes", "duration_ms"} {
		if _, present := ok[field]; !present {
			t.Errorf("audit line lacks %q: %v", field, ok)
		}
	}
	if ok["caller"] != "backup" || ok["operation"] != "list" || ok["path"] != "/backups/x" || ok["outcome"] != "ok" {
		t.Errorf("successful list line = %v", ok)
	}
	if b, _ := ok["bytes"].(float64); b <= 0 {
		t.Errorf("bytes = %v, want the response size", ok["bytes"])
	}
	if refused["caller"] != "backup" || refused["outcome"] != "forbidden" || refused["path"] != "/photos" {
		t.Errorf("out-of-Scope line = %v", refused)
	}
	for _, l := range []map[string]any{wrong, missing} {
		if _, has := l["caller"]; has {
			t.Errorf("rejected-auth line names a Caller: %v", l)
		}
		if l["outcome"] != "unauthorized" {
			t.Errorf("rejected-auth outcome = %v, want unauthorized", l["outcome"])
		}
	}
	for _, tok := range []string{adminToken, backupToken, wrongToken, adminToken[:16], backupToken[:16], wrongToken[:16]} {
		if strings.Contains(e.audit.String(), tok) {
			t.Errorf("audit log contains token material %q", tok)
		}
	}
}
