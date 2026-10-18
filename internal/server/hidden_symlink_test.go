package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// newEnvWithUpLink is an env whose photos/up is a symlink to the Upload Root, with
// the Staging Area and a DSM folder holding something.
func newEnvWithUpLink(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.write(".nas-file-api-staging/upload-1/data", "x")
	e.write("@eaDir/thumb.jpg", "x")
	e.write("photos/a.jpg", "x")
	if err := os.Symlink("..", filepath.Join(e.root, "photos", "up")); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestListingThroughASymlinkToTheUploadRootHidesTheStagingArea(t *testing.T) {
	e := newEnvWithUpLink(t)
	got := names(decodeListing(t, e.as(adminToken, "/api/v1/folders/photos/up")))
	if want := []string{"photos"}; !slices.Equal(got, want) {
		t.Errorf("listing through photos/up = %v, want %v", got, want)
	}
}

func TestHiddenEntriesReachedThroughASymlinkAre404(t *testing.T) {
	e := newEnvWithUpLink(t)
	for _, target := range []string{
		"/api/v1/folders/photos/up/.nas-file-api-staging",
		"/api/v1/folders/photos/up/@eaDir",
		"/api/v1/files/photos/up/.nas-file-api-staging",
		"/api/v1/files/photos/up/.nas-file-api-staging/upload-1/data",
		"/api/v1/files/photos/up/@eaDir/thumb.jpg?hash=sha256",
		"/api/v1/tree/photos/up/.nas-file-api-staging",
		"/api/v1/tree/photos/up/@eaDir",
	} {
		if rec := e.as(adminToken, target); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404; body %s", target, rec.Code, rec.Body)
		}
	}
	for _, target := range []string{
		"/api/v1/folders/photos/up/.nas-file-api-staging",
		"/api/v1/folders/photos/up/.nas-file-api-staging/new",
	} {
		if rec := e.put(adminToken, target); rec.Code != http.StatusNotFound {
			t.Errorf("PUT %s = %d, want 404; body %s", target, rec.Code, rec.Body)
		}
	}
	if e.isDir(".nas-file-api-staging/new") {
		t.Error("create-folder through a symlink made a folder in the Staging Area")
	}
	if rec := e.create(adminToken, 1, hello("photos/up/.nas-file-api-staging/x")); rec.Code != http.StatusNotFound {
		t.Errorf("Upload into the Staging Area through a symlink = %d, want 404; body %s", rec.Code, rec.Body)
	}
}
