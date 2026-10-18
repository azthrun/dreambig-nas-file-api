package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/azthrun/dreambig-nas-file-api/internal/config"
)

var envVarRe = regexp.MustCompile(`NAS_FILE_API_[A-Z0-9_]+`)

func varsIn(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return envVarRe.FindAllString(string(b), -1)
}

// Every NAS_FILE_API_* variable the service or compose.yaml uses is documented in
// .env.example, and .env.example documents nothing else.
func TestEnvExampleDocumentsExactlyTheVariablesInUse(t *testing.T) {
	documented := varsIn(t, ".env.example")
	used := append(slices.Clone(config.EnvVars), varsIn(t, "compose.yaml")...)

	for _, v := range used {
		if !slices.Contains(documented, v) {
			t.Errorf("%s is used but not documented in .env.example", v)
		}
	}
	for _, v := range documented {
		if !slices.Contains(used, v) {
			t.Errorf("%s is documented in .env.example but used neither by the service nor compose.yaml", v)
		}
	}
}
