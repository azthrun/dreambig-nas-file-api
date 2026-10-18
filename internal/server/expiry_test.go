package server_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const stagingDir = ".nas-file-api-staging"

const day = 24 * time.Hour

// untouchedFor makes the Upload at target look last touched ago (both its staged data
// and its info).
func (e *env) untouchedFor(target string, ago time.Duration) {
	e.t.Helper()
	id := path.Base(target)
	when := time.Now().Add(-ago)
	for _, name := range []string{id, id + ".info"} {
		if err := os.Chtimes(filepath.Join(e.root, stagingDir, name), when, when); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) expire() {
	e.t.Helper()
	if err := e.srv.ExpireUploads(context.Background()); err != nil {
		e.t.Fatalf("ExpireUploads: %v", err)
	}
}

func (e *env) staged(target string) bool {
	id := path.Base(target)
	return e.exists(filepath.Join(stagingDir, id)) || e.exists(filepath.Join(stagingDir, id+".info"))
}

func TestUploadUntouchedPastTheLimitIsRemovedAndAudited(t *testing.T) {
	e := newEnv(t)
	target := e.mustCreate(adminToken, 10, hello("photos/a.txt"))
	e.patch(adminToken, target, 0, []byte("half"))
	e.untouchedFor(target, 7*day+time.Hour)
	e.audit.Reset()

	e.expire()

	if e.staged(target) {
		t.Error("staged data of an abandoned Upload is still in the Staging Area")
	}
	if rec := e.tus(adminToken, http.MethodHead, target, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("HEAD after expiry = %d, want 404", rec.Code)
	}
	var found bool
	for _, l := range auditLines(t, e) {
		if l["operation"] == "upload_expire" {
			found = true
			if l["caller"] != "admin" || l["path"] != "/photos/a.txt" || l["outcome"] != "expired" || l["bytes"] != float64(4) {
				t.Errorf("expiry audit line = %v, want caller admin, path /photos/a.txt, outcome expired, bytes 4", l)
			}
		}
	}
	if !found {
		t.Errorf("no upload_expire audit line; got %v", auditLines(t, e))
	}
}

func TestUploadInsideTheLimitIsKeptAndCanStillResume(t *testing.T) {
	e := newEnv(t)
	data := []byte("0123456789")
	target := e.mustCreate(adminToken, len(data), hello("b.txt"))
	e.patch(adminToken, target, 0, data[:4])
	e.untouchedFor(target, 7*day-time.Hour)

	e.expire()

	if off := e.offset(adminToken, target); off != 4 {
		t.Fatalf("offset after expiry run = %d, want 4 (Upload kept)", off)
	}
	if rec := e.patch(adminToken, target, 4, data[4:]); rec.Code != http.StatusNoContent {
		t.Fatalf("resuming PATCH = %d, want 204; body %s", rec.Code, rec.Body)
	}
	if got := string(e.readFile("b.txt")); got != string(data) {
		t.Errorf("b.txt = %q, want %q", got, data)
	}
}

// An old Upload being resumed right now is in use, not abandoned: expiry neither
// removes it nor cuts off the request.
func TestUploadBeingResumedIsNotExpiredOrInterrupted(t *testing.T) {
	e := newEnv(t)
	data := []byte("0123456789")
	target := e.mustCreate(adminToken, len(data), hello("c.txt"))

	pr, pw := io.Pipe()
	h := http.Header{}
	h.Set("Upload-Offset", "0")
	h.Set("Content-Type", "application/offset+octet-stream")
	codes := make(chan int, 1)
	go func() { codes <- e.tus(adminToken, http.MethodPatch, target, h, pr).Code }()
	// tusd reads the body only once it holds the Upload's lock.
	if _, err := pw.Write(data[:4]); err != nil {
		t.Fatal(err)
	}
	// Wait for those bytes to land, so the age set below is the last touch.
	staged := filepath.Join(e.root, stagingDir, path.Base(target))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if fi, err := os.Stat(staged); err == nil && fi.Size() == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first bytes never reached the Staging Area")
		}
	}
	e.untouchedFor(target, 30*day)

	e.expire()

	if !e.staged(target) {
		t.Fatal("expiry removed an Upload a request was writing to")
	}
	if _, err := pw.Write(data[4:]); err != nil {
		t.Fatalf("request was cut off: %v", err)
	}
	pw.Close()
	if code := <-codes; code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204", code)
	}
	if got := string(e.readFile("c.txt")); got != string(data) {
		t.Errorf("c.txt = %q, want %q", got, data)
	}
}

func TestCompletedFilesAreNeverTouchedByExpiry(t *testing.T) {
	e := newEnv(t)
	data := []byte("done")
	target := e.mustCreate(adminToken, len(data), hello("done.txt"))
	if rec := e.patch(adminToken, target, 0, data); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204", rec.Code)
	}
	e.write("old/existing.txt", "a Caller's file")
	old := time.Now().Add(-365 * day)
	for _, rel := range []string{"done.txt", "old/existing.txt"} {
		if err := os.Chtimes(filepath.Join(e.root, rel), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A stale info whose data path points at a completed file must not make expiry
	// remove that file.
	id := "0123456789abcdef0123456789abcdef"
	info := `{"ID":"` + id + `","Size":10,"MetaData":{"path":"/x"},"Storage":{"Type":"filestore","Path":` +
		strconv.Quote(filepath.Join(e.root, "done.txt")) + `}}`
	e.write(filepath.Join(stagingDir, id+".info"), info)
	if err := os.Chtimes(filepath.Join(e.root, stagingDir, id+".info"), old, old); err != nil {
		t.Fatal(err)
	}

	e.expire()

	if got := string(e.readFile("done.txt")); got != "done" {
		t.Errorf("done.txt = %q after expiry, want it untouched", got)
	}
	if got := string(e.readFile("old/existing.txt")); got != "a Caller's file" {
		t.Errorf("old/existing.txt = %q after expiry, want it untouched", got)
	}
}
