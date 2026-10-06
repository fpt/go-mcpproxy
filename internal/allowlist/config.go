package allowlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// LoadConfig reads the config file at path. A missing file yields an empty
// config.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the user's own environment
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read allowlist config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse allowlist config %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes the config to path atomically, creating parent directories.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if c.Allow == nil {
		c.Allow = []string{}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode allowlist config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("write allowlist config: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write allowlist config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write allowlist config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write allowlist config: %w", err)
	}
	return nil
}

// Add appends entry unless an equivalent entry already exists.
// It reports whether the config changed.
func (c *Config) Add(entry string) bool {
	if c.index(entry) >= 0 {
		return false
	}
	c.Allow = append(c.Allow, entry)
	return true
}

// Remove deletes the entry equivalent to entry. It reports whether the
// config changed.
func (c *Config) Remove(entry string) bool {
	i := c.index(entry)
	if i < 0 {
		return false
	}
	c.Allow = slices.Delete(c.Allow, i, i+1)
	return true
}

func (c *Config) index(entry string) int {
	want := comparable(entry)
	return slices.IndexFunc(c.Allow, func(e string) bool {
		return e == entry || comparable(e) == want
	})
}

func comparable(entry string) string {
	p, err := expandHome(entry)
	if err != nil {
		return entry
	}
	return filepath.Clean(p)
}

// CanonicalEntry turns a command-line argument into the absolute path (or
// pattern) stored in the config: "~/" is expanded, relative paths are made
// absolute, and a bare command name is located via PATH. Symlinks are kept
// as given; they are resolved when the allowlist is loaded.
func CanonicalEntry(arg string) (string, error) {
	p, err := expandHome(strings.TrimSpace(arg))
	if err != nil {
		return "", err
	}
	if p == "" {
		return "", errors.New("empty executable path")
	}
	if !hasMeta(p) && !strings.ContainsRune(p, filepath.Separator) {
		found, err := exec.LookPath(p)
		if err != nil {
			return "", fmt.Errorf("locate %q: %w", p, err)
		}
		p = found
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("absolute path of %q: %w", p, err)
	}
	if _, err := normalizePattern(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// IsPattern reports whether entry contains wildcards.
func IsPattern(entry string) bool {
	return hasMeta(entry)
}

func hasMeta(p string) bool {
	return strings.ContainsAny(p, `*?[`)
}
