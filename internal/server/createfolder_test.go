package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// put sends PUT target with the given token.
func (e *env) put(token, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) isDir(rel string) bool {
	e.t.Helper()
	fi, err := os.Stat(filepath.Join(e.root, rel))
	return err == nil && fi.IsDir()
}

func TestCreateFolderMakesMissingParentsAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	first := e.put(adminToken, "/api/v1/folders/photos/2024/trip")
	if first.Code != http.StatusCreated {
		t.Fatalf("first PUT = %d, want 201; body %s", first.Code, first.Body)
	}
	if !e.isDir("photos/2024/trip") {
		t.Fatal("photos/2024/trip was not created")
	}
	second := e.put(adminToken, "/api/v1/folders/photos/2024/trip")
	if second.Code != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200; body %s", second.Code, second.Body)
	}
}

func TestCreateFolderStoresAnNFDNameInNFC(t *testing.T) {
	e := newEnv(t)
	// A macOS client sends NFD: "e" + combining acute accent.
	if rec := e.put(adminToken, "/api/v1/folders/cafe%CC%81"); rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201; body %s", rec.Code, rec.Body)
	}
	des, err := os.ReadDir(e.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 1 || des[0].Name() != "café" {
		var got []string
		for _, de := range des {
			got = append(got, fmt.Sprintf("%q", de.Name()))
		}
		t.Errorf("Upload Root holds %v, want the NFC name %q", got, "café")
	}
}

// Write-side path rules: names SMB cannot store and Hidden Entries are refused with 400,
// in any segment, and nothing is created.
func TestCreateFolderRefusesNamesBrokenByTheWriteRules(t *testing.T) {
	cases := map[string]string{
		"backslash":          "photos/a%5Cb",
		"colon":              "photos/a:b",
		"asterisk":           "photos/a*b",
		"question mark":      "photos/a%3Fb",
		"double quote":       "photos/a%22b",
		"less-than":          "photos/a%3Cb",
		"greater-than":       "photos/a%3Eb",
		"pipe":               "photos/a%7Cb",
		"trailing dot":       "photos/draft.",
		"trailing space":     "photos/draft%20",
		"rule in a parent":   "photos/bad:/ok",
		"@ segment":          "photos/@eaDir",
		"# segment":          "%23recycle/x",
		"Staging Area":       ".nas-file-api-staging/x",
		"@ in a later child": "photos/ok/@tmp",
	}
	for name, rel := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			rec := e.put(adminToken, "/api/v1/folders/"+rel)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400; body %s", rel, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), e.root) {
				t.Errorf("error body leaks the Upload Root's absolute path: %s", rec.Body)
			}
			if des, _ := os.ReadDir(e.root); len(des) != 0 {
				t.Errorf("PUT %s created %v", rel, des[0].Name())
			}
		})
	}
}

func TestCreateFolderAllowsDotsAndSpacesInsideNames(t *testing.T) {
	e := newEnv(t)
	if rec := e.put(adminToken, "/api/v1/folders/.config/my%20trip.2024"); rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201; body %s", rec.Code, rec.Body)
	}
}

func TestCreateFolderOutsideScopeIsRefusedAndAudited(t *testing.T) {
	e := newEnv(t)
	for _, rel := range []string{"photos/x", "backups-old", "Backups/x", ""} {
		if got := e.put(backupToken, "/api/v1/folders/"+rel).Code; got != http.StatusForbidden {
			t.Errorf("Scope /backups: PUT /%s = %d, want 403", rel, got)
		}
	}
	if des, _ := os.ReadDir(e.root); len(des) != 0 {
		t.Errorf("an out-of-Scope PUT created %v", des[0].Name())
	}
	lines := auditLines(t, e)
	if len(lines) != 4 {
		t.Fatalf("got %d audit lines, want 4: %s", len(lines), e.audit)
	}
	refused := lines[0]
	if refused["caller"] != "backup" || refused["operation"] != "create_folder" ||
		refused["path"] != "/photos/x" || refused["outcome"] != "forbidden" {
		t.Errorf("out-of-Scope audit line = %v", refused)
	}
}

func TestCreateFolderInsideScopeIsAudited(t *testing.T) {
	e := newEnv(t)
	if got := e.put(backupToken, "/api/v1/folders/backups/2024/q1").Code; got != http.StatusCreated {
		t.Fatalf("PUT inside Scope = %d, want 201", got)
	}
	if !e.isDir("backups/2024/q1") {
		t.Fatal("folder not created")
	}
	lines := auditLines(t, e)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want 1: %s", len(lines), e.audit)
	}
	l := lines[0]
	if l["caller"] != "backup" || l["operation"] != "create_folder" || l["path"] != "/backups/2024/q1" || l["outcome"] != "ok" {
		t.Errorf("audit line = %v", l)
	}
}

func TestCreateFolderThroughASymlinkOutOfScopeOrUploadRootIsRefused(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	e.mkdir("photos")
	e.mkdir("backups")
	if err := os.Symlink(outside, filepath.Join(e.root, "backups", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../photos", filepath.Join(e.root, "backups", "pics")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ token, rel string }{
		{adminToken, "backups/escape/new"},
		{backupToken, "backups/escape/new"},
		{backupToken, "backups/pics/new"},
	}
	for _, c := range cases {
		if got := e.put(c.token, "/api/v1/folders/"+c.rel).Code; got != http.StatusForbidden {
			t.Errorf("PUT %s = %d, want 403", c.rel, got)
		}
	}
	if des, _ := os.ReadDir(outside); len(des) != 0 {
		t.Errorf("created %v outside the Upload Root", des[0].Name())
	}
	if e.isDir("photos/new") {
		t.Error("created photos/new outside the Caller's Scope")
	}
}

func TestCreateFolderIsOwnedByTheService(t *testing.T) {
	e := newEnv(t)
	if got := e.put(adminToken, "/api/v1/folders/mine").Code; got != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", got)
	}
	fi, err := os.Stat(filepath.Join(e.root, "mine"))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no Unix ownership on this platform")
	}
	if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("owner = %d:%d, want the service's %d:%d", st.Uid, st.Gid, os.Getuid(), os.Getgid())
	}
}

func TestCreateFolderNeedsAValidCredential(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{"", wrongToken} {
		if got := e.put(tok, "/api/v1/folders/x").Code; got != http.StatusUnauthorized {
			t.Errorf("PUT with token %.4q... = %d, want 401", tok, got)
		}
	}
	if e.isDir("x") {
		t.Error("an unauthenticated PUT created a folder")
	}
}

func TestCreateFolderWhereAFileIsIsAConflict(t *testing.T) {
	e := newEnv(t)
	e.write("photos/a.jpg", "12345")
	for _, target := range []string{
		"/api/v1/folders/photos/a.jpg",
		"/api/v1/folders/photos/a.jpg/sub", // a file in place of a parent
	} {
		rec := e.put(adminToken, target)
		if rec.Code != http.StatusConflict {
			t.Errorf("PUT %s = %d, want 409; body %s", target, rec.Code, rec.Body)
		}
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "photos/a.jpg")); err != nil || string(b) != "12345" {
		t.Errorf("the file was touched: %q, %v", b, err)
	}
}
