package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// treeLine is one JSON line of GET /api/v1/tree/{path}.
type treeLine struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
	Error string `json:"error"`
}

func decodeTree(t *testing.T, rec *httptest.ResponseRecorder) []treeLine {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	var out []treeLine
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		var l treeLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q is not JSON: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

func treePaths(lines []treeLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Path)
	}
	slices.Sort(out)
	return out
}

func TestTreeStreamsEveryEntryWithRelativePathTypeSizeAndMtime(t *testing.T) {
	e := newEnv(t)
	e.write("photos/2024/a.jpg", "12345")
	e.write("photos/2024/trip/b.jpg", "xy")
	e.mkdir("photos/2024/empty")
	e.write("photos/other.txt", "x")
	mtime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(e.root, "photos/2024/a.jpg"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	lines := decodeTree(t, e.as(adminToken, "/api/v1/tree/photos/2024"))
	want := []string{"a.jpg", "empty", "trip", "trip/b.jpg"}
	if got := treePaths(lines); !slices.Equal(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for _, l := range lines {
		switch l.Path {
		case "a.jpg":
			if l.Type != "file" || l.Size != 5 || l.Mtime != "2024-06-01T12:00:00Z" {
				t.Errorf("a.jpg = %+v", l)
			}
		case "trip/b.jpg":
			if l.Type != "file" || l.Size != 2 {
				t.Errorf("trip/b.jpg = %+v", l)
			}
		case "empty", "trip":
			if l.Type != "folder" || l.Mtime == "" {
				t.Errorf("%s = %+v", l.Path, l)
			}
		}
	}

	if got := treePaths(decodeTree(t, e.as(adminToken, "/api/v1/tree/"))); !slices.Contains(got, "photos/2024/trip/b.jpg") {
		t.Errorf("tree of the Upload Root = %v, want it to include photos/2024/trip/b.jpg", got)
	}
}

func TestTreeNeverContainsHiddenEntriesOrTheirSubtrees(t *testing.T) {
	e := newEnv(t)
	e.write("@eaDir/thumb.jpg", "x")
	e.write("#recycle/old.txt", "x")
	e.write(".nas-file-api-staging/upload-1/data", "x")
	e.write("photos/@eaDir/a.jpg/SYNOPHOTO_THUMB.jpg", "x")
	e.write("photos/#snapshot", "x")
	e.write("photos/a.jpg", "x")
	e.write("photos/.nas-file-api-staging/kept.txt", "x") // only the Upload Root's one is the Staging Area

	want := []string{"photos", "photos/.nas-file-api-staging", "photos/.nas-file-api-staging/kept.txt", "photos/a.jpg"}
	if got := treePaths(decodeTree(t, e.as(adminToken, "/api/v1/tree/"))); !slices.Equal(got, want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
	for _, target := range []string{
		"/api/v1/tree/@eaDir",
		"/api/v1/tree/%23recycle",
		"/api/v1/tree/.nas-file-api-staging",
		"/api/v1/tree/photos/@eaDir",
	} {
		if got := e.as(adminToken, target).Code; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, got)
		}
	}
}

func TestTreeThroughASymlinkToTheUploadRootStillHidesTheStagingArea(t *testing.T) {
	e := newEnv(t)
	e.write(".nas-file-api-staging/upload-1/data", "x")
	e.write("photos/a.jpg", "x")
	if err := os.Symlink("..", filepath.Join(e.root, "photos", "up")); err != nil {
		t.Fatal(err)
	}
	rec := e.as(adminToken, "/api/v1/tree/photos/up")
	if rec.Code != http.StatusOK {
		return // refusing it outright is fine too
	}
	for _, p := range treePaths(decodeTree(t, rec)) {
		if strings.HasPrefix(p, ".nas-file-api-staging") {
			t.Errorf("tree through a symlink lists the Staging Area: %v", p)
		}
	}
}

func TestTreeReportsSymlinksWithoutFollowingThem(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.write("photos/a.jpg", "x")
	e.mkdir("backups")
	for name, dest := range map[string]string{"escape": outside, "loop": ".", "pics": "../photos"} {
		if err := os.Symlink(dest, filepath.Join(e.root, "backups", name)); err != nil {
			t.Fatal(err)
		}
	}
	lines := decodeTree(t, e.as(backupToken, "/api/v1/tree/backups"))
	if got := treePaths(lines); !slices.Equal(got, []string{"escape", "loop", "pics"}) {
		t.Errorf("tree = %v, want only the three symlinks", got)
	}
	for _, l := range lines {
		if l.Type != "symlink" {
			t.Errorf("%s type = %q, want symlink", l.Path, l.Type)
		}
	}
	if got := e.as(adminToken, "/api/v1/tree/backups/escape").Code; got != http.StatusForbidden {
		t.Errorf("tree of a symlink out of the Upload Root = %d, want 403", got)
	}
	if got := e.as(backupToken, "/api/v1/tree/backups/pics").Code; got != http.StatusForbidden {
		t.Errorf("tree of a symlink out of the Scope = %d, want 403", got)
	}
}

func TestTreeIsLimitedToTheCallersScope(t *testing.T) {
	e := newEnv(t)
	e.write("backups/x/db.dump", "data")
	e.write("photos/a.jpg", "x")
	if got := treePaths(decodeTree(t, e.as(backupToken, "/api/v1/tree/backups"))); !slices.Equal(got, []string{"x", "x/db.dump"}) {
		t.Errorf("tree of the Scope = %v", got)
	}
	for _, target := range []string{"/api/v1/tree", "/api/v1/tree/", "/api/v1/tree/photos", "/api/v1/tree/backups-old"} {
		if got := e.as(backupToken, target).Code; got != http.StatusForbidden {
			t.Errorf("Scope /backups: GET %s = %d, want 403", target, got)
		}
	}
}

func TestTreeErrors(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "x")
	e.mkdir("empty")
	if got := e.as(adminToken, "/api/v1/tree/nope").Code; got != http.StatusNotFound {
		t.Errorf("missing folder = %d, want 404", got)
	}
	if got := e.as(adminToken, "/api/v1/tree/a.txt").Code; got != http.StatusBadRequest {
		t.Errorf("tree of a file = %d, want 400", got)
	}
	if got := e.as(adminToken, "/api/v1/tree/empty/%2e%2e").Code; got != http.StatusBadRequest {
		t.Errorf("dot-dot = %d, want 400", got)
	}
	if lines := decodeTree(t, e.as(adminToken, "/api/v1/tree/empty")); len(lines) != 0 {
		t.Errorf("empty folder = %v, want no lines", lines)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tree/empty", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	if got := e.do("/api/v1/tree/empty", "").Code; got != http.StatusUnauthorized {
		t.Errorf("without Credential = %d, want 401", got)
	}
}

func TestTreeIsAudited(t *testing.T) {
	e := newEnv(t)
	e.write("backups/x/db.dump", "data")
	e.as(backupToken, "/api/v1/tree/backups/x")
	e.as(backupToken, "/api/v1/tree/photos")
	lines := auditLines(t, e)
	if len(lines) != 2 {
		t.Fatalf("audit lines = %v, want 2", lines)
	}
	if l := lines[0]; l["caller"] != "backup" || l["operation"] != "tree" || l["path"] != "/backups/x" || l["outcome"] != "ok" {
		t.Errorf("tree audit line = %v", l)
	}
	if l := lines[1]; l["operation"] != "tree" || l["outcome"] != "forbidden" || l["path"] != "/photos" {
		t.Errorf("refused tree audit line = %v", l)
	}
}

// disconnectingWriter plays a client that goes away after the first line: from then on
// the request context is cancelled (as net/http does when the connection drops) and,
// if failWrites, every write fails.
type disconnectingWriter struct {
	*httptest.ResponseRecorder
	cancel     context.CancelFunc
	failWrites bool
	writes     int
}

func (w *disconnectingWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes > 1 && w.failWrites {
		return 0, errors.New("connection reset by peer")
	}
	w.cancel()
	return w.ResponseRecorder.Write(b)
}

func TestClientDisconnectingMidStreamStopsTheWalk(t *testing.T) {
	e := newEnv(t)
	const total = 2000
	for i := range total {
		e.write(fmt.Sprintf("big/d%02d/f%04d", i%20, i), "")
	}
	for name, failWrites := range map[string]bool{"context cancelled": false, "writes fail": true} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/tree/big", nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			w := &disconnectingWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, failWrites: failWrites}
			e.h.ServeHTTP(w, req)
			if w.writes > 3 {
				t.Errorf("handler kept writing after the client left: %d writes for %d entries", w.writes, total)
			}
			lines := auditLines(t, e)
			if l := lines[len(lines)-1]; l["outcome"] != "aborted" {
				t.Errorf("audit line = %v, want outcome aborted", l)
			}
		})
	}
}

// heapSampler is a client that discards the body and, every `every` lines, samples the
// live heap after a GC.
type heapSampler struct {
	header       http.Header
	every, lines int
	base, peak   uint64
}

func (w *heapSampler) Header() http.Header { return w.header }
func (w *heapSampler) WriteHeader(int)     {}
func (w *heapSampler) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' {
			w.lines++
			if w.lines == 1 || w.lines%w.every == 0 {
				runtime.GC()
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if w.lines == 1 {
					w.base = m.HeapAlloc
				}
				w.peak = max(w.peak, m.HeapAlloc)
			}
		}
	}
	return len(b), nil
}

func TestTreeMemoryStaysFlatOnA100kEntryTree(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 100k files")
	}
	e := newEnv(t)
	// 60k files in one folder (so reading a folder whole would show) plus 40 folders of
	// 1k (so buffering the tree would show).
	create := func(rel string) {
		f, err := os.Create(filepath.Join(e.root, rel))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	e.mkdir("big/flat")
	for i := range 60_000 {
		create(fmt.Sprintf("big/flat/a-fairly-long-file-name-to-weigh-something-%06d.jpg", i))
	}
	for d := range 40 {
		e.mkdir(fmt.Sprintf("big/d%02d", d))
		for i := range 1000 {
			create(fmt.Sprintf("big/d%02d/a-fairly-long-file-name-to-weigh-something-%04d.jpg", d, i))
		}
	}
	const want = 1 + 60_000 + 40 + 40*1000

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tree/big", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := &heapSampler{header: http.Header{}, every: 5000}
	e.h.ServeHTTP(w, req)

	if w.lines != want {
		t.Fatalf("streamed %d lines, want %d", w.lines, want)
	}
	growth := int64(w.peak) - int64(w.base)
	t.Logf("heap growth while streaming %d entries: %d KiB", want, growth/1024)
	if growth > 2<<20 {
		t.Errorf("heap grew by %d KiB while streaming, want it flat (< 2 MiB)", growth/1024)
	}
}

func TestUnreadableFolderMidStreamEndsWithAnErrorLine(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	e := newEnv(t)
	e.write("photos/a.jpg", "x")
	e.write("photos/locked/b.jpg", "x")
	locked := filepath.Join(e.root, "photos", "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	lines := decodeTree(t, e.as(adminToken, "/api/v1/tree/photos"))
	if len(lines) == 0 || lines[len(lines)-1].Error == "" {
		t.Fatalf("lines = %+v, want a final error line", lines)
	}
	if last := lines[len(lines)-1]; strings.Contains(last.Error, e.root) {
		t.Errorf("error line leaks the NAS path: %q", last.Error)
	}
	if l := auditLines(t, e)[0]; l["outcome"] != "error" {
		t.Errorf("audit line = %v, want outcome error", l)
	}
}
