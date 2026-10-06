package subcmd

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/fpt/go-mcpproxy/internal/allowlist"
	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/auth"
	"github.com/fpt/go-mcpproxy/internal/remote"
)

// targetFlags are the flags describing how to reach an upstream server,
// shared by every subcommand that connects to one.
type targetFlags struct {
	transport string
	headers   stringList
	env       stringList
}

func (t *targetFlags) setFlags(f *flag.FlagSet) {
	f.StringVar(&t.transport, "transport", remote.TransportAuto,
		"Transport of a URL target: http or sse (default: sse if the path ends in /sse, else http)")
	f.Var(&t.headers, "header", `HTTP header "Name: value" sent to a URL target (repeatable)`)
	f.Var(&t.env, "env", "KEY=VALUE added to a command target's environment (repeatable)")
}

// remoteConfig builds the connection settings for a URL target.
func (t *targetFlags) remoteConfig(serverURL string) (remote.Config, error) {
	if _, err := remote.ResolveTransport(serverURL, t.transport); err != nil {
		return remote.Config{}, err
	}
	headers := make(map[string]string, len(t.headers))
	for _, h := range t.headers {
		name, value, err := remote.ParseHeader(h)
		if err != nil {
			return remote.Config{}, err
		}
		headers[name] = value
	}
	return remote.Config{URL: serverURL, Transport: t.transport, Headers: headers}, nil
}

// dialer builds a dialer for spec, which is either a single server URL or a
// command followed by its arguments. The allowlist is checked here so that a
// disallowed target fails immediately, and again on every connection.
func (t *targetFlags) dialer(e *environment, spec []string) (app.Dialer, error) {
	if len(spec) == 0 {
		return nil, errors.New("no target: give a server URL or -- <command> [args...]")
	}
	if remote.IsURL(spec[0]) {
		if len(spec) > 1 {
			return nil, fmt.Errorf("unexpected arguments after URL: %q", spec[1:])
		}
		cfg, err := t.remoteConfig(spec[0])
		if err != nil {
			return nil, err
		}
		if err := e.allow.CheckURL(cfg.URL); err != nil {
			return nil, e.rejected(err)
		}
		return app.NewRemoteDialer(cfg, e.allow.CheckURL, e.creds.OAuthConfig), nil
	}
	if _, err := e.allow.Resolve(spec[0]); err != nil {
		return nil, e.rejected(err)
	}
	return app.NewStdioDialer(spec[0], spec[1:], t.env, e.allow.Resolve)
}

// targetName is a short name for a target: the command's base name or the
// URL's host.
func targetName(spec []string) string {
	if len(spec) == 0 {
		return ""
	}
	if remote.IsURL(spec[0]) {
		if u, err := url.Parse(spec[0]); err == nil {
			return u.Host
		}
	}
	return filepath.Base(spec[0])
}

// environment is the user-level state: the allowlist and stored credentials.
type environment struct {
	cfgPath string
	allow   *allowlist.Allowlist
	creds   *auth.Store
}

func loadEnvironment() (*environment, error) {
	path, err := allowlist.DefaultPath()
	if err != nil {
		return nil, err
	}
	a, err := allowlist.Load(path)
	if err != nil {
		return nil, err
	}
	return &environment{
		cfgPath: path,
		allow:   a,
		creds:   auth.NewStore(filepath.Join(filepath.Dir(path), "credentials")),
	}, nil
}

func (e *environment) rejected(err error) error {
	if errors.Is(err, allowlist.ErrNotAllowed) {
		return fmt.Errorf("%w (config: %s; allow it with `mcpproxy add`)", err, e.cfgPath)
	}
	return err
}
