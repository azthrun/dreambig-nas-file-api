// Package config reads the service's NAS_FILE_API_* settings. No other package reads
// the environment.
package config

// Vars lists every environment variable the service itself reads. compose.yaml-only
// variables (UID/GID, Tailscale) are not here; the env-docs test reads those from
// compose.yaml.
var Vars = []string{envAddr}

const (
	envAddr     = "NAS_FILE_API_ADDR"
	defaultAddr = ":8080"
)

// Config is the service's runtime configuration.
type Config struct {
	// Addr is the host:port the HTTP server listens on.
	Addr string
}

// Load builds a Config from getenv (os.Getenv in production).
func Load(getenv func(string) string) Config {
	c := Config{Addr: getenv(envAddr)}
	if c.Addr == "" {
		c.Addr = defaultAddr
	}
	return c
}
