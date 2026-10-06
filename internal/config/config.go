// Package config loads and edits the mcpproxy configuration: the set of
// backend MCP servers that mcpproxy serves. Only servers listed here are ever
// launched or contacted, so the file doubles as the allowlist. It lives in a
// user-level file, outside of any project, so that a project-local MCP
// configuration cannot widen what the proxy runs.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/fpt/go-mcpproxy/internal/remote"
)

// EnvPath overrides the default config file location.
const EnvPath = "MCPPROXY_CONFIG"

// ToolSeparator joins a server name and a tool name in the names exposed to
// clients, e.g. "godev__search_godoc". Server names may not contain it.
const ToolSeparator = "__"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// Config is the on-disk configuration.
type Config struct {
	// Servers maps a server name to how to reach it.
	Servers map[string]Server `toml:"servers"`
}

// Server is one backend MCP server: either a local stdio command or a
// remote URL.
type Server struct {
	// Command is the absolute path of a stdio server executable.
	Command string `toml:"command,omitempty"`
	// Args are passed to Command.
	Args []string `toml:"args,omitempty"`
	// Env is added to the environment of Command. Values may reference
	// environment variables as ${NAME}.
	Env map[string]string `toml:"env,omitempty"`
	// Watch lists extra files whose modification restarts the server. The
	// executable itself is always watched.
	Watch []string `toml:"watch,omitempty"`

	// URL is the endpoint of a streamable HTTP or SSE server.
	URL string `toml:"url,omitempty"`
	// Transport is "http" or "sse"; empty picks by URL path.
	Transport string `toml:"transport,omitempty"`
	// Headers are sent with every request. Values may reference environment
	// variables as ${NAME}, which keeps secrets out of this file.
	Headers map[string]string `toml:"headers,omitempty"`
}

// IsRemote reports whether the server is reached by URL.
func (s Server) IsRemote() bool { return s.URL != "" }

// String describes the server's target for humans.
func (s Server) String() string {
	if s.IsRemote() {
		return s.URL
	}
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}

// Equal reports whether two server definitions are identical.
func (s Server) Equal(o Server) bool {
	return s.Command == o.Command && slices.Equal(s.Args, o.Args) &&
		mapsEqual(s.Env, o.Env) && slices.Equal(s.Watch, o.Watch) &&
		s.URL == o.URL && s.Transport == o.Transport && mapsEqual(s.Headers, o.Headers)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Validate checks a server definition.
func (s Server) Validate() error {
	switch {
	case s.Command != "" && s.URL != "":
		return errors.New("set either command or url, not both")
	case s.Command == "" && s.URL == "":
		return errors.New("command or url is required")
	case s.IsRemote():
		if len(s.Args) > 0 || len(s.Env) > 0 {
			return errors.New("args and env apply only to command servers")
		}
		if _, err := remote.ResolveTransport(s.URL, s.Transport); err != nil {
			return err
		}
		for name := range s.Headers {
			if _, _, err := remote.ParseHeader(name + ": x"); err != nil {
				return fmt.Errorf("invalid header name %q", name)
			}
		}
	default:
		if !filepath.IsAbs(s.Command) {
			return fmt.Errorf("command %q must be an absolute path", s.Command)
		}
		if s.Transport != "" || len(s.Headers) > 0 {
			return errors.New("transport and headers apply only to url servers")
		}
		for _, w := range s.Watch {
			if !filepath.IsAbs(w) {
				return fmt.Errorf("watch path %q must be absolute", w)
			}
		}
	}
	return nil
}

// ValidateName checks a server name. Names become tool name prefixes, so
// they are restricted to letters, digits, "-" and "_", without "__".
func ValidateName(name string) error {
	if !namePattern.MatchString(name) || strings.Contains(name, ToolSeparator) {
		return fmt.Errorf("invalid server name %q: use up to 32 letters, digits, '-' or '_' "+
			"(starting with a letter or digit, without %q)", name, ToolSeparator)
	}
	return nil
}

// Validate checks every server and name.
func (c *Config) Validate() error {
	for _, name := range c.Names() {
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := c.Servers[name].Validate(); err != nil {
			return fmt.Errorf("server %q: %w", name, err)
		}
	}
	return nil
}

// Names returns the server names in sorted order.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// DefaultPath returns the config file path: $MCPPROXY_CONFIG if set,
// otherwise $XDG_CONFIG_HOME/mcpproxy/config.toml, falling back to
// ~/.config/mcpproxy/config.toml.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvPath); p != "" {
		return expandHome(p)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mcpproxy", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "mcpproxy", "config.toml"), nil
}

// Load reads and validates the config file at path. A missing file yields an
// empty config. Unknown keys are rejected so that typos do not silently
// disable a setting.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the user's own environment
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Servers: map[string]Server{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]Server{}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes the config atomically with 0600 permissions (it may contain
// headers with credentials), creating parent directories. Comments in a
// hand-edited file are not preserved.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(
		"# mcpproxy configuration. Edit by hand or with `mcpproxy add` / `mcpproxy rm`.\n" +
			"# A running `mcpproxy serve` reloads this file automatically.\n\n",
	)
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// ResolveCommand turns a command-line argument into the absolute command
// path stored in the config: "~/" is expanded, relative paths are made
// absolute, and a bare name is located via PATH. Symlinks are kept, so that
// a symlink retargeted by a build is followed on the next restart.
func ResolveCommand(arg string) (string, error) {
	p, err := expandHome(strings.TrimSpace(arg))
	if err != nil {
		return "", err
	}
	if p == "" {
		return "", errors.New("empty command")
	}
	if !strings.ContainsRune(p, filepath.Separator) {
		found, err := exec.LookPath(p)
		if err != nil {
			return "", fmt.Errorf("locate %q: %w", p, err)
		}
		p = found
	}
	return filepath.Abs(p)
}

// AbsPath expands "~/" and makes p absolute.
func AbsPath(p string) (string, error) {
	p, err := expandHome(strings.TrimSpace(p))
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
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

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces ${NAME} references with environment variables. A bare
// "$" is left alone, so values containing dollar signs need no escaping.
func ExpandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}
