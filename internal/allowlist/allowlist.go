// Package allowlist restricts which executables mcpproxy is permitted to launch.
//
// The allowlist lives in a user-level config file, outside of any project, so
// that a project-local MCP configuration cannot widen what the proxy will run.
package allowlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnvConfigPath overrides the default config file location.
const EnvConfigPath = "MCPPROXY_CONFIG"

// ErrNotAllowed is returned when an executable does not match any allowlist entry.
var ErrNotAllowed = errors.New("executable is not in the mcpproxy allowlist")

// Config is the on-disk configuration format.
type Config struct {
	// Allow lists executable path patterns. Each entry is an absolute path
	// (a leading "~/" is expanded) that may contain filepath.Match wildcards.
	// A trailing "/**" matches any file below that directory.
	Allow []string `json:"allow"`
}

// Allowlist matches executables against configured patterns.
type Allowlist struct {
	patterns []string
}

// DefaultPath returns the config file path: $MCPPROXY_CONFIG if set,
// otherwise $XDG_CONFIG_HOME/mcpproxy/config.json, falling back to
// ~/.config/mcpproxy/config.json.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return expandHome(p)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mcpproxy", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "mcpproxy", "config.json"), nil
}

// Load reads the config file at path. A missing file yields an empty
// allowlist, which rejects everything.
func Load(path string) (*Allowlist, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the user's own environment
	if errors.Is(err, os.ErrNotExist) {
		return New(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("read allowlist config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse allowlist config %s: %w", path, err)
	}
	return New(cfg.Allow)
}

// New builds an allowlist from raw patterns.
func New(patterns []string) (*Allowlist, error) {
	a := &Allowlist{}
	for _, p := range patterns {
		norm, err := normalizePattern(p)
		if err != nil {
			return nil, err
		}
		a.patterns = append(a.patterns, norm)
	}
	return a, nil
}

// Patterns returns the normalized patterns.
func (a *Allowlist) Patterns() []string {
	return append([]string(nil), a.patterns...)
}

// Resolve locates command (via PATH if it has no separator), resolves it to
// an absolute path with symlinks evaluated, and checks it against the
// allowlist. It returns the resolved path on success.
func (a *Allowlist) Resolve(command string) (string, error) {
	found, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("locate %q: %w", command, err)
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return "", fmt.Errorf("absolute path of %q: %w", found, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks of %q: %w", abs, err)
	}
	if a.Match(real) {
		return real, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNotAllowed, real)
}

// Match reports whether the absolute, symlink-free path matches any pattern.
func (a *Allowlist) Match(path string) bool {
	for _, p := range a.patterns {
		if matchPattern(p, path) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, path string) bool {
	if dir, ok := strings.CutSuffix(pattern, string(filepath.Separator)+"**"); ok {
		// Any ancestor directory of path may match; dir may contain wildcards.
		for d := filepath.Dir(path); ; d = filepath.Dir(d) {
			if ok, _ := filepath.Match(dir, d); ok {
				return true
			}
			if d == filepath.Dir(d) {
				return false
			}
		}
	}
	ok, err := filepath.Match(pattern, path)
	return err == nil && ok
}

// normalizePattern expands "~", requires an absolute path, and resolves
// symlinks in the longest wildcard-free directory prefix so that patterns
// compare equal to symlink-resolved executable paths (e.g. /tmp on macOS).
func normalizePattern(p string) (string, error) {
	p, err := expandHome(strings.TrimSpace(p))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("allowlist pattern %q must be an absolute path", p)
	}
	p = filepath.Clean(p)
	if _, err := filepath.Match(strings.TrimSuffix(p, "/**"), ""); err != nil {
		return "", fmt.Errorf("allowlist pattern %q: %w", p, err)
	}

	prefix, rest := splitLiteralPrefix(p)
	if real, err := filepath.EvalSymlinks(prefix); err == nil {
		prefix = real
	}
	if rest == "" {
		return prefix, nil
	}
	return filepath.Join(prefix, rest), nil
}

// splitLiteralPrefix splits p into the longest directory prefix containing
// no wildcard characters and the remainder.
func splitLiteralPrefix(p string) (string, string) {
	idx := strings.IndexAny(p, `*?[\`)
	if idx < 0 {
		return p, ""
	}
	dir := filepath.Dir(p[:idx+1])
	rest, _ := filepath.Rel(dir, p)
	return dir, rest
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
