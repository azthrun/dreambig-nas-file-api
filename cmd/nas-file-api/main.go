// Command nas-file-api serves the NAS file API.
//
// With -healthcheck it instead probes /healthz on the configured address and exits
// 0 or 1, so the distroless image (no shell, no wget) can run its own HEALTHCHECK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/config"
	"github.com/azthrun/dreambig-nas-file-api/internal/server"
	"github.com/azthrun/dreambig-nas-file-api/internal/upload"
	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on NAS_FILE_API_ADDR and exit 0 if it answers 200")
	flag.Parse()

	cfg, cfgErr := config.Load(os.Getenv)
	if *healthcheck {
		// Only Addr is needed; Load sets it even when the rest of the config is bad.
		if err := probe(cfg.Addr); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if cfgErr != nil {
		log.Error("refusing to start: bad config", "err", cfgErr)
		os.Exit(1)
	}
	if err := run(cfg, log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root, err := uploadroot.Open(cfg.UploadRoot)
	if err != nil {
		return err
	}
	defer root.Close()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	// The audit log goes to stdout, operational logs to stderr.
	handler, err := server.New(server.Deps{
		Root: root, Callers: cfg.Callers, Audit: os.Stdout, Log: log,
		Limits: upload.Limits{
			MaxActivePerCaller: cfg.MaxActiveUploadsPerCaller,
			FreeSpaceReserve:   cfg.FreeSpaceReserve,
		},
		UploadExpiry: cfg.UploadExpiry,
	})
	if err != nil {
		return err
	}
	// Abandoned Uploads are removed at startup and then hourly, until shutdown.
	expiryCtx, stopExpiry := context.WithCancel(ctx)
	expiryDone := make(chan struct{})
	go func() {
		defer close(expiryDone)
		handler.RunUploadExpiry(expiryCtx, time.Hour)
	}()
	defer func() {
		stopExpiry()
		<-expiryDone
	}()
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "callers", len(cfg.Callers))

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// probe GETs /healthz on addr, reaching a wildcard listen address via loopback.
func probe(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse NAS_FILE_API_ADDR %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("GET /healthz: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz answered %d", resp.StatusCode)
	}
	return nil
}
