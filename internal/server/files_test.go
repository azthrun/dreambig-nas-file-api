package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fileStat struct {
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	Size   int64   `json:"size"`
	Mtime  string  `json:"mtime"`
	SHA256 *string `json:"sha256"`
}

func decodeStat(t *testing.T, rec *httptest.ResponseRecorder) fileStat {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var s fileStat
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	return s
}

func TestStatFileReturnsTypeSizeAndMtimeWithoutHash(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a.jpg", "12345")
	mtime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(e.root, "photos/a.jpg"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	s := decodeStat(t, e.as(adminToken, "/api/v1/files/photos/a.jpg"))
	if s.Path != "/photos/a.jpg" || s.Type != "file" || s.Size != 5 || s.Mtime != "2024-06-01T12:00:00Z" {
		t.Errorf("stat = %+v", s)
	}
	if s.SHA256 != nil {
		t.Errorf("sha256 = %q without ?hash=sha256, want absent", *s.SHA256)
	}
}

func TestStatWithHashReturnsSHA256(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a.jpg", "12345")
	e.write("empty.txt", "")
	for target, want := range map[string]string{
		// Expected values from `sha256sum`.
		"/api/v1/files/photos/a.jpg?hash=sha256": "5994471abb01112afcc18159f6cc74b4f511b99806da59b3caf5a9c173cacfc5",
		"/api/v1/files/empty.txt?hash=sha256":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	} {
		s := decodeStat(t, e.as(adminToken, target))
		if s.SHA256 == nil || *s.SHA256 != want {
			t.Errorf("GET %s: sha256 = %v, want %s", target, s.SHA256, want)
		}
	}
}

func TestStatWithHashStreamsAMultiGBFile(t *testing.T) {
	if testing.Short() {
		t.Skip("hashes 3 GiB")
	}
	e := newEnv(t)
	e.mkdir("backups")
	// Sparse: 3 GiB of zeros then "end", taking almost no disk space.
	f, err := os.Create(filepath.Join(e.root, "backups", "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	const size = 3 << 30
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("end"), size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s := decodeStat(t, e.as(backupToken, "/api/v1/files/backups/disk.img?hash=sha256"))
	runtime.ReadMemStats(&after)

	// Expected value from `sha256sum` over the same file.
	const want = "fc2ebca1af84292658bd8421e7b095bf977b0eeb44478e89096448c2784bf9a8"
	if s.SHA256 == nil || *s.SHA256 != want || s.Size != size+3 {
		t.Errorf("stat = %+v (sha256 %v), want size %d and sha256 %s", s, s.SHA256, size+3, want)
	}
	// Loading the file would allocate at least its size; streaming allocates a buffer.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("hashing allocated %d MiB, want it streamed (< 64 MiB)", alloc>>20)
	}
}

func TestStatWithHashRefusesFoldersAndUnknownAlgorithms(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a.jpg", "12345")
	for _, target := range []string{
		"/api/v1/files/photos?hash=sha256",
		"/api/v1/files/?hash=sha256",
		"/api/v1/files/photos/a.jpg?hash=md5",
		"/api/v1/files/photos/a.jpg?hash=",
		"/api/v1/files/photos/a.jpg?hash=sha256&hash=md5",
	} {
		if got := e.as(adminToken, target).Code; got != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, got)
		}
	}
}

func TestStatWithHashOfAnInScopeSymlinkIsRefused(t *testing.T) {
	// A symlink is reported, not followed, as in a listing; there is no file to hash.
	e := newEnv(t)
	e.write("photos/a.jpg", "12345")
	if err := os.Symlink("a.jpg", filepath.Join(e.root, "photos", "link.jpg")); err != nil {
		t.Fatal(err)
	}
	if s := decodeStat(t, e.as(adminToken, "/api/v1/files/photos/link.jpg")); s.Type != "symlink" {
		t.Errorf("stat of a symlink = %+v, want type symlink", s)
	}
	if got := e.as(adminToken, "/api/v1/files/photos/link.jpg?hash=sha256").Code; got != http.StatusBadRequest {
		t.Errorf("hash of a symlink = %d, want 400", got)
	}
}

func TestStatFolderAndUploadRoot(t *testing.T) {
	e := newEnv(t)
	e.mkdir("photos/2024")
	if s := decodeStat(t, e.as(adminToken, "/api/v1/files/photos/2024")); s.Path != "/photos/2024" || s.Type != "folder" || s.Size != 0 {
		t.Errorf("folder stat = %+v", s)
	}
	for _, target := range []string{"/api/v1/files/", "/api/v1/files"} {
		if s := decodeStat(t, e.as(adminToken, target)); s.Path != "/" || s.Type != "folder" {
			t.Errorf("GET %s = %+v, want the Upload Root as a folder", target, s)
		}
	}
}

func TestStatMissingPathIs404(t *testing.T) {
	e := newEnv(t)
	e.mkdir("photos")
	for _, target := range []string{"/api/v1/files/nope", "/api/v1/files/photos/nope.jpg", "/api/v1/files/nope/deeper"} {
		if got := e.as(adminToken, target).Code; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, got)
		}
	}
}

func TestStatIsOnlyGET(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "x")
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
		req := httptest.NewRequest(m, "/api/v1/files/a.txt", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET" {
			t.Errorf("%s: Allow = %q, want GET", m, got)
		}
	}
}

func TestStatNeedsACredential(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "x")
	if got := e.do("/api/v1/files/a.txt", "").Code; got != http.StatusUnauthorized {
		t.Errorf("no Credential = %d, want 401", got)
	}
	if got := e.as(wrongToken, "/api/v1/files/a.txt").Code; got != http.StatusUnauthorized {
		t.Errorf("wrong Credential = %d, want 401", got)
	}
}

func TestStatHiddenEntriesAre404(t *testing.T) {
	e := newEnv(t)
	e.mkdir("@eaDir")
	e.mkdir("#recycle")
	e.write(".nas-file-api-staging/upload-1", "partial")
	e.write("photos/@eaDir/a.jpg/SYNOFILE_THUMB_M.jpg", "x")
	for _, target := range []string{
		"/api/v1/files/@eaDir",
		"/api/v1/files/%23recycle",
		"/api/v1/files/.nas-file-api-staging",
		"/api/v1/files/.nas-file-api-staging/upload-1",
		"/api/v1/files/photos/@eaDir/a.jpg/SYNOFILE_THUMB_M.jpg",
		"/api/v1/files/.nas-file-api-staging/upload-1?hash=sha256",
	} {
		if got := e.as(adminToken, target).Code; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, got)
		}
	}
}

func TestStatOutsideScopeIsRefused(t *testing.T) {
	e := newEnv(t)
	e.write("backups/x/db.dump", "data")
	e.write("photos/a.jpg", "x")
	if s := decodeStat(t, e.as(backupToken, "/api/v1/files/backups/x/db.dump")); s.Type != "file" || s.Size != 4 {
		t.Errorf("in-Scope stat = %+v", s)
	}
	for _, target := range []string{
		"/api/v1/files/",
		"/api/v1/files/photos/a.jpg",
		"/api/v1/files/photos/a.jpg?hash=sha256",
		"/api/v1/files/nope", // refused before revealing whether it exists
	} {
		if got := e.as(backupToken, target).Code; got != http.StatusForbidden {
			t.Errorf("Scope /backups: GET %s = %d, want 403", target, got)
		}
	}
}

func TestStatPathValidation(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a.jpg", "x")
	for _, target := range []string{
		"/api/v1/files/photos/../photos/a.jpg",
		"/api/v1/files/photos/%2e%2e/photos/a.jpg",
		"/api/v1/files//etc/passwd",
		"/api/v1/files/photos%2Fa.jpg",
		"/api/v1/files/photos/",
	} {
		rec := e.as(adminToken, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), e.root) {
			t.Errorf("GET %s leaks the Upload Root's absolute path: %s", target, rec.Body)
		}
	}
}

func TestStatSymlinksOutOfTheUploadRootOrScopeAreRefused(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.write("photos/a.jpg", "x")
	e.mkdir("backups")
	for link, dest := range map[string]string{
		"photos/escape":   outside,
		"photos/secret":   filepath.Join(outside, "secret.txt"),
		"backups/pics":    "../photos",
		"backups/pic.jpg": "../photos/a.jpg",
	} {
		if err := os.Symlink(dest, filepath.Join(e.root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ token, target string }{
		{adminToken, "/api/v1/files/photos/escape/secret.txt"},
		{adminToken, "/api/v1/files/photos/secret"},
		{adminToken, "/api/v1/files/photos/secret?hash=sha256"},
		{backupToken, "/api/v1/files/backups/pics/a.jpg"},
		{backupToken, "/api/v1/files/backups/pic.jpg?hash=sha256"},
	} {
		rec := e.as(c.token, c.target)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", c.target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), outside) {
			t.Errorf("GET %s leaks the outside folder: %s", c.target, rec.Body)
		}
	}
}

func TestStatAuditsCallerOperationAndPath(t *testing.T) {
	e := newEnv(t)
	e.write("backups/db.dump", "data")
	e.as(backupToken, "/api/v1/files/backups/db.dump?hash=sha256")
	e.as(backupToken, "/api/v1/files/photos/a.jpg")

	lines := auditLines(t, e)
	if len(lines) != 2 {
		t.Fatalf("got %d audit lines, want 2: %s", len(lines), e.audit)
	}
	if l := lines[0]; l["caller"] != "backup" || l["operation"] != "stat" || l["path"] != "/backups/db.dump" || l["outcome"] != "ok" {
		t.Errorf("stat line = %v", l)
	}
	if l := lines[1]; l["caller"] != "backup" || l["operation"] != "stat" || l["path"] != "/photos/a.jpg" || l["outcome"] != "forbidden" {
		t.Errorf("refused stat line = %v", l)
	}
}
