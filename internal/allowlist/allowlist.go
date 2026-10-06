// Package allowlist restricts which executables mcpproxy is permitted to launch.
//
// The allowlist lives in a user-level config file, outside of any project, so
// that a project-local MCP configuration cannot widen what the proxy will run.
package allowlist

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fpt/go-mcpproxy/internal/remote"
)

// EnvConfigPath overrides the default config file location.
const EnvConfigPath = "MCPPROXY_CONFIG"

// ErrNotAllowed is returned when an executable or URL does not match any
// allowlist entry.
var ErrNotAllowed = errors.New("not in the mcpproxy allowlist")

// Config is the on-disk configuration format.
type Config struct {
	// Allow lists executable path patterns and server URLs.
	//
	// A path entry is absolute (a leading "~/" is expanded) and may contain
	// filepath.Match wildcards; a trailing "/**" matches any file below that
	// directory. A URL entry (http:// or https://) matches that exact URL,
	// ignoring query and fragment; a trailing "*" makes it a prefix match.
	Allow []string `json:"allow"`
}

// Allowlist matches executables against configured patterns.
type Allowlist struct {
	patterns []string
	urls     []string
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
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return New(cfg.Allow)
}

// New builds an allowlist from raw patterns.
func New(patterns []string) (*Allowlist, error) {
	a := &Allowlist{}
	for _, p := range patterns {
		if remote.IsURL(strings.TrimSpace(p)) {
			norm, err := normalizeURLPattern(p)
			if err != nil {
				return nil, err
			}
			a.urls = append(a.urls, norm)
			continue
		}
		norm, err := normalizePattern(p)
		if err != nil {
			return nil, err
		}
		a.patterns = append(a.patterns, norm)
	}
	return a, nil
}

// Patterns returns the normalized path patterns followed by URL patterns.
func (a *Allowlist) Patterns() []string {
	return append(append([]string(nil), a.patterns...), a.urls...)
}

// CheckURL returns an error wrapping ErrNotAllowed unless serverURL matches
// a URL entry.
func (a *Allowlist) CheckURL(serverURL string) error {
	target, err := normalizeURL(serverURL)
	if err != nil {
		return err
	}
	for _, p := range a.urls {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(target, prefix) {
				return nil
			}
		} else if target == p {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotAllowed, target)
}

// normalizeURL lowercases scheme and host and drops query, fragment and a
// trailing slash, so that equivalent spellings compare equal.
func normalizeURL(s string) (string, error) {
	u, err := remote.ParseURL(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return strings.TrimSuffix(u.String(), "/"), nil
}

func normalizeURLPattern(p string) (string, error) {
	p = strings.TrimSpace(p)
	base, wildcard := strings.CutSuffix(p, "*")
	norm, err := normalizeURL(base)
	if err != nil {
		return "", fmt.Errorf("allowlist entry %q: %w", p, err)
	}
	if wildcard {
		if strings.HasSuffix(base, "/") {
			norm += "/"
		}
		return norm + "*", nil
	}
	return norm, nil
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
