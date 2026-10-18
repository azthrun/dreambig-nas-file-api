// Package config reads the service's NAS_FILE_API_* settings. No other package reads
// the environment.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azthrun/dreambig-nas-file-api/internal/uploadroot"
)

// EnvVars lists every environment variable the service itself reads. compose.yaml-only
// variables (UID/GID, Tailscale) are not here; the env-docs test reads those from
// compose.yaml.
var EnvVars = []string{envAddr, envUploadRoot, envCallers, envUploadExpiryDays}

const (
	envAddr       = "NAS_FILE_API_ADDR"
	envUploadRoot = "NAS_FILE_API_UPLOAD_ROOT"
	envCallers    = "NAS_FILE_API_CALLERS"
	// envUploadExpiryDays is how many days an incomplete Upload may go untouched
	// before it is removed from the Staging Area.
	envUploadExpiryDays     = "NAS_FILE_API_UPLOAD_EXPIRY_DAYS"
	defaultUploadExpiryDays = 7
	defaultAddr             = ":8080"
	defaultUploadRoot       = "/data"

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
		Addr:       orDefault(getenv(envAddr), defaultAddr),
		UploadRoot: orDefault(getenv(envUploadRoot), defaultUploadRoot),
	}
	callers, err := parseCallers(getenv(envCallers))
	if err != nil {
		return c, fmt.Errorf("%s: %w", envCallers, err)
	}
	c.Callers = callers
	days := defaultUploadExpiryDays
	if v := getenv(envUploadExpiryDays); v != "" {
		if days, err = strconv.Atoi(v); err != nil || days < 1 {
			return c, fmt.Errorf("%s: want a whole number of days, at least 1", envUploadExpiryDays)
		}
	}
	c.UploadExpiry = time.Duration(days) * 24 * time.Hour
	return c, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
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
