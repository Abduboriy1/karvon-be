package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// loadDotenv applies KEY=VALUE lines from path to the process environment without
// overwriting variables that are already set. A missing file is not an error, so the
// same code path works in development and in a container that only has real env vars.
func loadDotenv(path string) error {
	// path comes from the process's own configuration, not from user input.
	f, err := os.Open(path) //nolint:gosec // G304: caller-controlled config path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := parseDotenvLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return sc.Err()
}

// parseDotenvLine splits a single .env line. It reports ok=false for blanks and comments.
func parseDotenvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}
