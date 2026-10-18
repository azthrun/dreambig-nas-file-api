package config_test

import (
	"strings"
	"testing"
	"time"

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
	if cfg.FreeSpaceReserve != 20_000_000_000 {
		t.Errorf("FreeSpaceReserve = %d, want 20 GB", cfg.FreeSpaceReserve)
	}
	if cfg.MaxActiveUploadsPerCaller != 4 {
		t.Errorf("MaxActiveUploadsPerCaller = %d, want 4", cfg.MaxActiveUploadsPerCaller)
	}
}

func TestLoadUploadLimits(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"NAS_FILE_API_CALLERS":                       "laptop:" + tokenA,
		"NAS_FILE_API_FREE_SPACE_RESERVE_GB":         "0",
		"NAS_FILE_API_MAX_ACTIVE_UPLOADS_PER_CALLER": "16",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FreeSpaceReserve != 0 || cfg.MaxActiveUploadsPerCaller != 16 {
		t.Errorf("FreeSpaceReserve = %d, MaxActiveUploadsPerCaller = %d; want 0, 16", cfg.FreeSpaceReserve, cfg.MaxActiveUploadsPerCaller)
	}
	cfg, err = config.Load(env(map[string]string{
		"NAS_FILE_API_CALLERS":               "laptop:" + tokenA,
		"NAS_FILE_API_FREE_SPACE_RESERVE_GB": "500",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FreeSpaceReserve != 500_000_000_000 {
		t.Errorf("FreeSpaceReserve = %d, want 500 GB", cfg.FreeSpaceReserve)
	}
}

func TestLoadRefusesBadUploadLimits(t *testing.T) {
	cases := map[string]map[string]string{
		"reserve not a number":   {"NAS_FILE_API_FREE_SPACE_RESERVE_GB": "20GB"},
		"negative reserve":       {"NAS_FILE_API_FREE_SPACE_RESERVE_GB": "-1"},
		"overflowing reserve":    {"NAS_FILE_API_FREE_SPACE_RESERVE_GB": "99999999999"},
		"zero Uploads":           {"NAS_FILE_API_MAX_ACTIVE_UPLOADS_PER_CALLER": "0"},
		"Uploads not a number":   {"NAS_FILE_API_MAX_ACTIVE_UPLOADS_PER_CALLER": "four"},
		"negative Uploads limit": {"NAS_FILE_API_MAX_ACTIVE_UPLOADS_PER_CALLER": "-4"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			m["NAS_FILE_API_CALLERS"] = "laptop:" + tokenA
			if _, err := config.Load(env(m)); err == nil {
				t.Fatalf("Load(%v) succeeded, want error", m)
			}
		})
	}
}

func TestUploadExpiryDefaultsToSevenDaysAndIsConfigurableInDays(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"NAS_FILE_API_CALLERS": "laptop:" + tokenA}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadExpiry != 7*24*time.Hour {
		t.Errorf("default UploadExpiry = %v, want 168h", cfg.UploadExpiry)
	}
	cfg, err = config.Load(env(map[string]string{
		"NAS_FILE_API_CALLERS":            "laptop:" + tokenA,
		"NAS_FILE_API_UPLOAD_EXPIRY_DAYS": "30",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadExpiry != 30*24*time.Hour {
		t.Errorf("UploadExpiry = %v, want 720h", cfg.UploadExpiry)
	}
}

func TestLoadRefusesBadUploadExpiry(t *testing.T) {
	for _, v := range []string{"0", "-1", "7d", "1.5", "abc"} {
		_, err := config.Load(env(map[string]string{
			"NAS_FILE_API_CALLERS":            "laptop:" + tokenA,
			"NAS_FILE_API_UPLOAD_EXPIRY_DAYS": v,
		}))
		if err == nil {
			t.Errorf("Load with NAS_FILE_API_UPLOAD_EXPIRY_DAYS=%q succeeded, want error", v)
		}
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
