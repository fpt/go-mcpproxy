package subcmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/fpt/go-mcpproxy/internal/auth"
	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/hub"
)

// environment is the user-level state: the config file and stored
// credentials.
type environment struct {
	cfgPath string
	creds   *auth.Store
}

func newEnvironment() (*environment, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return nil, err
	}
	return &environment{
		cfgPath: path,
		creds:   auth.NewStore(filepath.Join(filepath.Dir(path), "credentials")),
	}, nil
}

func (e *environment) load() (*config.Config, error) {
	return config.Load(e.cfgPath)
}

// server returns the configured server with the given name.
func (e *environment) server(name string) (config.Server, error) {
	cfg, err := e.load()
	if err != nil {
		return config.Server{}, err
	}
	srv, ok := cfg.Servers[name]
	if !ok {
		names := cfg.Names()
		if len(names) == 0 {
			return config.Server{}, fmt.Errorf("no server named %q; none are configured "+
				"(add one with `mcpproxy add`)", name)
		}
		return config.Server{}, fmt.Errorf("no server named %q; configured: %s",
			name, strings.Join(names, ", "))
	}
	return srv, nil
}

func (e *environment) hubOptions(stderr io.Writer) hub.Options {
	return hub.Options{
		Settle:        300 * time.Millisecond,
		StartTimeout:  30 * time.Second,
		Stderr:        stderr,
		Creds:         e.creds,
		ClientVersion: Version,
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
