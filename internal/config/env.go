package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// LoadEnvFile loads a dotenv file into the process environment and returns its
// path, or "" when no file was used.
//
// Order: the explicit path (must exist), then $OPTICORE_ENV_FILE (must exist),
// then ./.env, then .env beside the executable. The last two are optional, so a
// container that passes real variables needs no file at all.
//
// Variables already set in the environment win over the file: godotenv does not
// override them, so `FOO=1 opticore serve` beats FOO in .env.
func LoadEnvFile(explicit string) (string, error) {
	return loadEnvFile(explicit, os.Executable)
}

func loadEnvFile(explicit string, exe func() (string, error)) (string, error) {
	if explicit != "" {
		return explicit, load(explicit)
	}
	if p := os.Getenv("OPTICORE_ENV_FILE"); p != "" {
		return p, load(p)
	}
	candidates := []string{".env"}
	if p, err := exe(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(p), ".env"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		return p, load(p)
	}
	return "", nil
}

func load(path string) error {
	if err := godotenv.Load(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("env file %s: not found", path)
		}
		return fmt.Errorf("env file %s: %w", path, err)
	}
	return nil
}
