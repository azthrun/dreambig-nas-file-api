package config_test

import (
	"strings"
	"testing"

	"github.com/azthrun/dreambig-nas-file-api/internal/config"
)

// Well-formed Credentials (64 hex characters, as `openssl rand -hex 32` prints), built
// from patterns so no secret-looking literal sits in the repo.
var (
	tokenA = strings.Repeat("0123456789abcdef", 4)
	tokenB = strings.Repeat("fedcba9876543210", 4)
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadParsesCallersWithOptionalScope(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"NAS_FILE_API_CALLERS": "laptop:" + tokenA + ", backup-script:" + tokenB + ":/backups/nightly",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Callers) != 2 {
		t.Fatalf("got %d Callers, want 2", len(cfg.Callers))
	}
	laptop, script := cfg.Callers[0], cfg.Callers[1]
	if laptop.Name != "laptop" || laptop.Token != tokenA || laptop.Scope.String() != "/" {
		t.Errorf("laptop = %q %q scope %q", laptop.Name, laptop.Token, laptop.Scope)
	}
	if script.Name != "backup-script" || script.Token != tokenB || script.Scope.String() != "/backups/nightly" {
		t.Errorf("backup-script = %q %q scope %q", script.Name, script.Token, script.Scope)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"NAS_FILE_API_CALLERS": "laptop:" + tokenA}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.UploadRoot != "/data" {
		t.Errorf("UploadRoot = %q, want /data", cfg.UploadRoot)
	}
}

// Load refuses to start the service on any Callers value that would leave it guessable
// or ambiguous (ADR-0004). Errors never echo the token.
func TestLoadRefusesBadCallers(t *testing.T) {
	cases := map[string]string{
		"missing":           "",
		"placeholder token": "laptop:<token>",
		"short token":       "laptop:3f9c1e7a5b2d4f60",
		"non-hex token":     "laptop:" + strings.Repeat("z", 64),
		"repeated char":     "laptop:" + strings.Repeat("a", 64),
		"no token":          "laptop",
		"empty name":        ":" + tokenA,
		"bad name":          "my laptop:" + tokenA,
		"duplicate names":   "laptop:" + tokenA + ",laptop:" + tokenB,
		"duplicate tokens":  "laptop:" + tokenA + ",phone:" + tokenA,
		"scope with ..":     "laptop:" + tokenA + ":/backups/../photos",
		"relative scope":    "laptop:" + tokenA + ":backups",
		"hidden scope":      "laptop:" + tokenA + ":/@eaDir",
		"empty entry":       "laptop:" + tokenA + ",,phone:" + tokenB,
	}
	for name, callers := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(env(map[string]string{"NAS_FILE_API_CALLERS": callers}))
			if err == nil {
				t.Fatalf("Load(%q) succeeded, want error", callers)
			}
			if strings.Contains(err.Error(), tokenA) || strings.Contains(err.Error(), tokenB) {
				t.Errorf("error leaks a token: %v", err)
			}
		})
	}
}
