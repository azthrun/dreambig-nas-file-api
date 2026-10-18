// Package config reads the service's NAS_FILE_API_* settings. No other package reads
// the environment.
package config

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// EnvVars lists every environment variable the service itself reads. compose.yaml-only
// variables (UID/GID, Tailscale) are not here; the env-docs test reads those from
// compose.yaml.
var EnvVars = []string{envAddr, envUploadRoot, envCallers, envFreeSpaceReserveGB, envMaxActiveUploads, envUploadExpiryDays}

const (
	envAddr               = "NAS_FILE_API_ADDR"
	envUploadRoot         = "NAS_FILE_API_UPLOAD_ROOT"
	envCallers            = "NAS_FILE_API_CALLERS"
	envFreeSpaceReserveGB = "NAS_FILE_API_FREE_SPACE_RESERVE_GB"
	envMaxActiveUploads   = "NAS_FILE_API_MAX_ACTIVE_UPLOADS_PER_CALLER"
	// envUploadExpiryDays is how many days an incomplete Upload may go untouched
	// before it is removed from the Staging Area.
	envUploadExpiryDays     = "NAS_FILE_API_UPLOAD_EXPIRY_DAYS"
	defaultAddr             = ":8080"
	defaultUploadRoot       = "/data"
	defaultReserveGB        = 20
	defaultMaxActive        = 4
	defaultUploadExpiryDays = 7

	// gb is a decimal gigabyte, as `df -H` and most disk labels count.
	gb = 1_000_000_000

	// minTokenHexLen is what `openssl rand -hex 32` prints (ADR-0004).
	minTokenHexLen = 64
)

var callerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Config is the service's runtime configuration.
type Config struct {
	// Addr is the host:port the HTTP server listens on. Load sets it even when it
	// returns an error, so the healthcheck probe can run without the rest.
	Addr string
	// UploadRoot is the OS path of the Upload Root (inside the container).
	UploadRoot string
	// Callers are the Callers allowed to use the API, in config order.
	Callers []Caller
	// FreeSpaceReserve is the free space, in bytes, that Uploads must leave on the
	// Upload Root's volume.
	FreeSpaceReserve int64
	// MaxActiveUploadsPerCaller is how many unfinished Uploads one Caller may have.
	MaxActiveUploadsPerCaller int
	// UploadExpiry is how long an incomplete Upload may go untouched before it is
	// removed from the Staging Area.
	UploadExpiry time.Duration
}

// Caller is one configured Caller (GLOSSARY.md).
type Caller struct {
	// Name identifies the Caller in the audit log.
	Name string
	// Token is the Caller's Credential. Never log or echo it.
	Token string
	// Scope is the subtree the Caller may list and write; the zero Path is the whole
	// Upload Root.
	Scope uploadroot.Path
}

// Load builds a Config from getenv (os.Getenv in production). It refuses Callers
// config that is missing, malformed, uses placeholder or short tokens, or repeats a
// name or token (ADR-0004). Errors never contain token material.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:       cmp.Or(getenv(envAddr), defaultAddr),
		UploadRoot: cmp.Or(getenv(envUploadRoot), defaultUploadRoot),
	}
	callers, err := parseCallers(getenv(envCallers))
	if err != nil {
		return c, fmt.Errorf("%s: %w", envCallers, err)
	}
	c.Callers = callers
	reserveGB, err := parseInt(getenv(envFreeSpaceReserveGB), defaultReserveGB, 0, math.MaxInt64/gb)
	if err != nil {
		return c, fmt.Errorf("%s: %w", envFreeSpaceReserveGB, err)
	}
	c.FreeSpaceReserve = reserveGB * gb
	maxActive, err := parseInt(getenv(envMaxActiveUploads), defaultMaxActive, 1, math.MaxInt32)
	if err != nil {
		return c, fmt.Errorf("%s: %w", envMaxActiveUploads, err)
	}
	c.MaxActiveUploadsPerCaller = int(maxActive)
	expiryDays, err := parseInt(getenv(envUploadExpiryDays), defaultUploadExpiryDays, 1, math.MaxInt64/int64(24*time.Hour))
	if err != nil {
		return c, fmt.Errorf("%s: %w", envUploadExpiryDays, err)
	}
	c.UploadExpiry = time.Duration(expiryDays) * 24 * time.Hour
	return c, nil
}

// parseInt parses a whole number in [lo, hi], or returns def for "".
func parseInt(s string, def, lo, hi int64) (int64, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("want a whole number from %d to %d, got %q", lo, hi, s)
	}
	return n, nil
}

// parseCallers parses "name:token[:/scope],name:token[:/scope],...".
func parseCallers(s string) ([]Caller, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("no Callers configured")
	}
	var callers []Caller
	names := map[string]bool{}
	tokens := map[string]bool{}
	for i, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("entry %d is empty", i+1)
		}
		parts := strings.SplitN(entry, ":", 3)
		name := parts[0]
		if !callerNameRe.MatchString(name) {
			return nil, fmt.Errorf("entry %d: Caller name must be 1-64 of A-Z a-z 0-9 . _ - (starting with a letter or digit)", i+1)
		}
		if len(parts) < 2 {
			return nil, fmt.Errorf("Caller %q: missing token (want name:token[:/scope])", name)
		}
		token := parts[1]
		if err := checkToken(token); err != nil {
			return nil, fmt.Errorf("Caller %q: %w", name, err)
		}
		var scope uploadroot.Path
		if len(parts) == 3 {
			var err error
			if scope, err = uploadroot.ParseScope(parts[2]); err != nil {
				return nil, fmt.Errorf("Caller %q: Scope: %w", name, err)
			}
		}
		if names[name] {
			return nil, fmt.Errorf("Caller %q is configured twice", name)
		}
		if tokens[token] {
			return nil, fmt.Errorf("Caller %q reuses another Caller's token", name)
		}
		names[name], tokens[token] = true, true
		callers = append(callers, Caller{Name: name, Token: token, Scope: scope})
	}
	return callers, nil
}

func checkToken(token string) error {
	if len(token) < minTokenHexLen {
		return fmt.Errorf("token is shorter than %d hex characters; generate one with `openssl rand -hex 32`", minTokenHexLen)
	}
	if _, err := hex.DecodeString(token); err != nil {
		return errors.New("token is not hex (a placeholder?); generate one with `openssl rand -hex 32`")
	}
	if strings.Count(token, token[:1]) == len(token) {
		return errors.New("token is one repeated character (a placeholder?); generate one with `openssl rand -hex 32`")
	}
	return nil
}
