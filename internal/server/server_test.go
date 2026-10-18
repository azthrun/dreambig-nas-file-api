package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/azthrun/dreambig-nas-file-api/internal/server"
)

func get(t *testing.T, path string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(server.New())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHealthzAnswers200WithoutCredential(t *testing.T) {
	if got := get(t, "/healthz").StatusCode; got != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", got)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	if got := get(t, "/nope").StatusCode; got != http.StatusNotFound {
		t.Fatalf("GET /nope = %d, want 404", got)
	}
}
