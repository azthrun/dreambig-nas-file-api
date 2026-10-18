package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const uploadsURL = "/api/v1/uploads/"

// meta is tus Upload-Metadata: key → plain value (encoded by tusMeta).
type meta map[string]string

func tusMeta(m meta) string {
	var pairs []string
	for k, v := range m {
		pairs = append(pairs, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(pairs, ",")
}

// tus sends one tus request with the given token.
func (e *env) tus(token, method, target string, header http.Header, body io.Reader) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	for k, vs := range header {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// create starts an Upload of size bytes and returns the response.
func (e *env) create(token string, size int, m meta) *httptest.ResponseRecorder {
	e.t.Helper()
	h := http.Header{}
	h.Set("Upload-Length", strconv.Itoa(size))
	h.Set("Upload-Metadata", tusMeta(m))
	return e.tus(token, http.MethodPost, uploadsURL, h, nil)
}

// mustCreate starts an Upload and returns its URL path.
func (e *env) mustCreate(token string, size int, m meta) string {
	e.t.Helper()
	rec := e.create(token, size, m)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("POST = %d, want 201; body %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	i := strings.Index(loc, uploadsURL)
	if i < 0 {
		e.t.Fatalf("Location %q is not under %s", loc, uploadsURL)
	}
	return loc[i:]
}

// patch sends data at offset to the Upload at target.
func (e *env) patch(token, target string, offset int, data []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	h := http.Header{}
	h.Set("Upload-Offset", strconv.Itoa(offset))
	h.Set("Content-Type", "application/offset+octet-stream")
	return e.tus(token, http.MethodPatch, target, h, bytes.NewReader(data))
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (e *env) readFile(rel string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.root, rel))
	return err == nil
}

var testMtime = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

func TestUploadCompletesAtTheDestinationPathWithItsMtimeAndReturnsTheHash(t *testing.T) {
	e := newEnv(t)
	data := []byte("hello, NAS")
	target := e.mustCreate(adminToken, len(data), meta{
		"path":  "photos/2021/hello.txt",
		"mtime": testMtime.Format(time.RFC3339),
	})
	rec := e.patch(adminToken, target, 0, data)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204; body %s", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("Upload-Sha256"), sha256Hex(data); got != want {
		t.Errorf("Upload-Sha256 = %q, want %q", got, want)
	}
	if got := e.readFile("photos/2021/hello.txt"); !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}
	fi, err := os.Stat(filepath.Join(e.root, "photos/2021/hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(testMtime) {
		t.Errorf("mtime = %v, want %v", fi.ModTime().UTC(), testMtime)
	}
}

func hello(path string) meta {
	return meta{"path": path, "mtime": testMtime.Format(time.RFC3339)}
}

// failingBody yields data, then fails as a dropped connection would.
type failingBody struct{ r io.Reader }

func (b *failingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (e *env) offset(token, target string) int {
	e.t.Helper()
	rec := e.tus(token, http.MethodHead, target, nil, nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("HEAD = %d, want 200", rec.Code)
	}
	n, err := strconv.Atoi(rec.Header().Get("Upload-Offset"))
	if err != nil {
		e.t.Fatalf("Upload-Offset %q: %v", rec.Header().Get("Upload-Offset"), err)
	}
	return n
}

func TestInterruptedUploadResumesAndCompletes(t *testing.T) {
	e := newEnv(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 256*1024) // 4 MiB
	target := e.mustCreate(adminToken, len(data), hello("big.bin"))

	// The connection drops halfway through the first PATCH.
	h := http.Header{}
	h.Set("Upload-Offset", "0")
	h.Set("Content-Type", "application/offset+octet-stream")
	e.tus(adminToken, http.MethodPatch, target, h, &failingBody{r: bytes.NewReader(data[:len(data)/2])})
	if e.exists("big.bin") {
		t.Fatal("an incomplete Upload is visible at its Destination Path")
	}

	off := e.offset(adminToken, target)
	if off != len(data)/2 {
		t.Fatalf("offset after interruption = %d, want %d", off, len(data)/2)
	}
	if rec := e.patch(adminToken, target, off, data[off:]); rec.Code != http.StatusNoContent {
		t.Fatalf("resuming PATCH = %d, want 204; body %s", rec.Code, rec.Body)
	}
	if got := e.readFile("big.bin"); !bytes.Equal(got, data) {
		t.Fatalf("completed file differs from the uploaded data (%d vs %d bytes)", len(got), len(data))
	}
}

func TestStagingAreaNeverAppearsInListingsAndIsNoDestination(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 10, hello("a.txt"))
	e.patch(adminToken, target, 0, []byte("half"))

	if !e.exists(".nas-file-api-staging") {
		t.Fatal("test assumption: the Staging Area exists under the Upload Root")
	}
	if got := names(decodeListing(t, e.as(adminToken, "/api/v1/folders/"))); len(got) != 0 {
		t.Errorf("Upload Root listing = %v, want nothing (staged data is hidden)", got)
	}
	if rec := e.as(adminToken, "/api/v1/folders/.nas-file-api-staging"); rec.Code != http.StatusNotFound {
		t.Errorf("listing the Staging Area = %d, want 404", rec.Code)
	}
	if rec := e.create(adminToken, 1, hello(".nas-file-api-staging/x")); rec.Code != http.StatusBadRequest {
		t.Errorf("Upload into the Staging Area = %d, want 400", rec.Code)
	}
}

func TestRacingUploadsToOnePathOneWinsTheOtherIsAConflict(t *testing.T) {
	e := newEnv(t)
	a, b := []byte("first contender"), []byte("second contender")
	ta := e.mustCreate(adminToken, len(a), hello("same.txt"))
	tb := e.mustCreate(adminToken, len(b), hello("same.txt"))
	e.patch(adminToken, ta, 0, a[:4])
	e.patch(adminToken, tb, 0, b[:4])

	codes := make(chan int, 2)
	go func() { codes <- e.patch(adminToken, ta, 4, a[4:]).Code }()
	go func() { codes <- e.patch(adminToken, tb, 4, b[4:]).Code }()
	got := []int{<-codes, <-codes}

	wins, conflicts := 0, 0
	for _, c := range got {
		switch c {
		case http.StatusNoContent:
			wins++
		case http.StatusConflict:
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("final PATCH statuses = %v, want one 204 and one 409", got)
	}
	if content := e.readFile("same.txt"); !bytes.Equal(content, a) && !bytes.Equal(content, b) {
		t.Errorf("same.txt = %q, want one contender's data whole", content)
	}
}

func TestWrongDeclaredHashLeavesNothingAtTheDestination(t *testing.T) {
	e := newEnv(t)
	data := []byte("payload")
	m := hello("x/y.txt")
	m["sha256"] = sha256Hex([]byte("something else"))
	target := e.mustCreate(adminToken, len(data), m)
	rec := e.patch(adminToken, target, 0, data)
	if rec.Code != 460 {
		t.Fatalf("PATCH = %d, want 460 (checksum mismatch); body %s", rec.Code, rec.Body)
	}
	if e.exists("x/y.txt") || e.exists("x") {
		t.Error("something was created for an Upload with a wrong hash")
	}
	if rec := e.tus(adminToken, http.MethodHead, target, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("HEAD after a hash mismatch = %d, want 404 (staged data discarded)", rec.Code)
	}
}

func TestRightDeclaredHashInAnyCaseIsAccepted(t *testing.T) {
	e := newEnv(t)
	data := []byte("payload")
	m := hello("y.txt")
	m["sha256"] = strings.ToUpper(sha256Hex(data))
	target := e.mustCreate(adminToken, len(data), m)
	if rec := e.patch(adminToken, target, 0, data); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204; body %s", rec.Code, rec.Body)
	}
}

func TestEmptyFileUploadCompletesOnCreation(t *testing.T) {
	e := newEnv(t)
	rec := e.create(adminToken, 0, hello("empty.txt"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201; body %s", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("Upload-Sha256"), sha256Hex(nil); got != want {
		t.Errorf("Upload-Sha256 = %q, want %q", got, want)
	}
	if got := e.readFile("empty.txt"); len(got) != 0 {
		t.Errorf("empty.txt = %q", got)
	}
}

func TestUploadOntoAnExistingEntryIsAConflictAtCreation(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a.jpg", "original")
	e.write("file", "x")
	e.mkdir("folder")
	for _, p := range []string{"photos/a.jpg", "folder", "file/child.txt"} {
		if rec := e.create(adminToken, 3, hello(p)); rec.Code != http.StatusConflict {
			t.Errorf("Upload to %s = %d, want 409", p, rec.Code)
		}
	}
	if got := string(e.readFile("photos/a.jpg")); got != "original" {
		t.Errorf("existing file changed to %q", got)
	}
}

func TestFileAppearingDuringAnUploadIsAConflictAndIsNotOverwritten(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 3, hello("late.txt"))
	e.write("late.txt", "someone else's")
	if rec := e.patch(adminToken, target, 0, []byte("new")); rec.Code != http.StatusConflict {
		t.Fatalf("PATCH = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if got := string(e.readFile("late.txt")); got != "someone else's" {
		t.Errorf("late.txt = %q, was overwritten", got)
	}
	if rec := e.tus(adminToken, http.MethodHead, target, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("HEAD after a Conflict = %d, want 404 (staged data discarded)", rec.Code)
	}
}

func TestUploadMetadataIsValidated(t *testing.T) {
	mt := testMtime.Format(time.RFC3339)
	cases := map[string]meta{
		"missing path":     {"mtime": mt},
		"missing mtime":    {"path": "a.txt"},
		"bad mtime":        {"path": "a.txt", "mtime": "yesterday"},
		"bad sha256":       {"path": "a.txt", "mtime": mt, "sha256": "abc"},
		"dot segment":      {"path": "../a.txt", "mtime": mt},
		"SMB-unstorable":   {"path": "a:b.txt", "mtime": mt},
		"trailing dot":     {"path": "a.", "mtime": mt},
		"hidden entry":     {"path": "@eaDir/a.txt", "mtime": mt},
		"the Upload Root":  {"path": "/", "mtime": mt},
		"absolute OS path": {"path": "//etc/passwd", "mtime": mt},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			rec := e.create(adminToken, 1, m)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d, want 400; body %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), e.root) {
				t.Errorf("error echoes the NAS path: %s", rec.Body)
			}
		})
	}
}

func TestLeadingSlashInTheDestinationPathIsAccepted(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 1, hello("/slash.txt"))
	if rec := e.patch(adminToken, target, 0, []byte("x")); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204", rec.Code)
	}
	if !e.exists("slash.txt") {
		t.Error("slash.txt missing")
	}
}

func TestUploadOutsideScopeIsRefused(t *testing.T) {
	e := newEnv(t)
	if rec := e.create(backupToken, 1, hello("photos/a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("out-of-Scope Upload = %d, want 403", rec.Code)
	}
	target := e.mustCreate(backupToken, 1, hello("backups/db/a.dump"))
	if rec := e.patch(backupToken, target, 0, []byte("x")); rec.Code != http.StatusNoContent {
		t.Fatalf("in-Scope PATCH = %d, want 204", rec.Code)
	}
	if !e.exists("backups/db/a.dump") {
		t.Error("in-Scope Upload missing")
	}
}

func TestUploadThroughASymlinkOutOfScopeIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mkdir("photos")
	e.mkdir("backups")
	if err := os.Symlink(filepath.Join(e.root, "photos"), filepath.Join(e.root, "backups/link")); err != nil {
		t.Fatal(err)
	}
	if rec := e.create(backupToken, 1, hello("backups/link/a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("Upload through a symlink out of Scope = %d, want 403", rec.Code)
	}
}

func TestOnlyTheCreatingCallerCanTouchAnUpload(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 4, hello("backups/mine.txt"))
	for _, method := range []string{http.MethodHead, http.MethodDelete} {
		if rec := e.tus(backupToken, method, target, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s by another Caller = %d, want 404", method, rec.Code)
		}
	}
	if rec := e.patch(backupToken, target, 0, []byte("evil")); rec.Code != http.StatusNotFound {
		t.Errorf("PATCH by another Caller = %d, want 404", rec.Code)
	}
	if got := e.offset(adminToken, target); got != 0 {
		t.Errorf("offset = %d, want 0", got)
	}
}

// ADR-0001: nothing is read back, not even staged data.
func TestUploadsCannotBeDownloaded(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 4, hello("a.txt"))
	e.patch(adminToken, target, 0, []byte("se"))
	if rec := e.tus(adminToken, http.MethodGet, target, nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on an Upload = %d %q, want 405", rec.Code, rec.Body)
	}
	h := http.Header{}
	h.Set("X-HTTP-Method-Override", http.MethodGet)
	if rec := e.tus(adminToken, http.MethodPost, target, h, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET via method override = %d %q, want 405", rec.Code, rec.Body)
	}
}

func TestCancellingAnUploadRemovesOnlyStagedData(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 4, hello("a.txt"))
	e.patch(adminToken, target, 0, []byte("ab"))
	if rec := e.tus(adminToken, http.MethodDelete, target, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if rec := e.tus(adminToken, http.MethodHead, target, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("HEAD after DELETE = %d, want 404", rec.Code)
	}
	des, _ := os.ReadDir(filepath.Join(e.root, ".nas-file-api-staging"))
	if len(des) != 0 {
		t.Errorf("Staging Area still holds %d entries", len(des))
	}
}

func TestUploadsNeedACredential(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodPost, uploadsURL, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "1")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without Credential = %d, want 401", rec.Code)
	}
	if rec := e.create(wrongToken, 1, hello("a.txt")); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST with a wrong Credential = %d, want 401", rec.Code)
	}
	if e.exists(".nas-file-api-staging") {
		t.Error("an unauthenticated request touched the Staging Area")
	}
}

func TestUnknownUploadIDIs404(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{strings.Repeat("ab", 16), "..", "x.info", "%2e%2e%2fsecret"} {
		if rec := e.tus(adminToken, http.MethodHead, uploadsURL+id, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("HEAD %s = %d, want 404", id, rec.Code)
		}
	}
}

func TestUploadsAreAuditedWithCallerAndDestinationPath(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(backupToken, 4, hello("backups/a.txt"))
	e.patch(backupToken, target, 0, []byte("data"))
	e.create(backupToken, 1, hello("photos/b.txt"))

	lines := auditLines(t, e)
	if len(lines) != 3 {
		t.Fatalf("got %d audit lines, want 3: %s", len(lines), e.audit)
	}
	created, completed, refused := lines[0], lines[1], lines[2]
	if created["caller"] != "backup" || created["operation"] != "upload_create" || created["path"] != "/backups/a.txt" || created["outcome"] != "ok" {
		t.Errorf("create line = %v", created)
	}
	if completed["caller"] != "backup" || completed["operation"] != "upload" || completed["path"] != "/backups/a.txt" ||
		completed["outcome"] != "completed" || completed["bytes"] != float64(4) {
		t.Errorf("completing PATCH line = %v", completed)
	}
	if refused["caller"] != "backup" || refused["path"] != "/photos/b.txt" || refused["outcome"] != "forbidden" {
		t.Errorf("refused create line = %v", refused)
	}
}

// Over a real connection, as behind `tailscale serve`: the Location URL a client
// follows is https on the public host.
func TestLocationHonoursTheProxysForwardedHeaders(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+uploadsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "1")
	req.Header.Set("Upload-Metadata", tusMeta(hello("a.txt")))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "nas-file-api.example.ts.net")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://nas-file-api.example.ts.net"+uploadsURL) {
		t.Errorf("Location = %q", loc)
	}
}
