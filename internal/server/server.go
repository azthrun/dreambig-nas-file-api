// Package server builds the HTTP handler for the NAS file API.
package server

import "net/http"

// New returns the service's HTTP handler.
func New() http.Handler {
	mux := http.NewServeMux()
	// Liveness only; no Credential needed, reveals nothing about the Upload Root.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
