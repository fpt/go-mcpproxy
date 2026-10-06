package subcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fpt/go-mcpproxy/internal/auth"
	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/google/subcommands"
)

// openBrowser opens the authorization URL; tests replace it.
var openBrowser = auth.OpenBrowser

// remoteServer returns the configured URL server with the given name.
func remoteServer(env *environment, name string) (config.Server, error) {
	srv, err := env.server(name)
	if err != nil {
		return config.Server{}, err
	}
	if !srv.IsRemote() {
		return config.Server{}, fmt.Errorf("server %q is a command, not a URL; "+
			"only URL servers use authorization", name)
	}
	return srv, nil
}

// AuthCmd authorizes mcpproxy to access a remote MCP server via OAuth.
type AuthCmd struct {
	clientID     string
	clientSecret string
	scopes       stringList
	port         int
	noBrowser    bool
	force        bool
	timeout      time.Duration
}

func (*AuthCmd) Name() string     { return "auth" }
func (*AuthCmd) Synopsis() string { return "Authorize access to a URL server (OAuth)." }
func (*AuthCmd) Usage() string {
	return `auth [flags] <name>:
  Run the OAuth authorization flow in the browser for the configured URL
  server <name> and store the credentials, which "serve", "tools" and "call"
  then use (refreshing the token when needed). A running "mcpproxy serve"
  picks them up on its next reconnect.

  The client is registered dynamically unless -client-id is given. For a
  server that takes a static token instead, configure a header:
    mcpproxy add -header 'Authorization: Bearer ${TOKEN}' <name> <url>

`
}

func (p *AuthCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.clientID, "client-id", "", "Pre-registered OAuth client ID")
	f.StringVar(&p.clientSecret, "client-secret", "", "OAuth client secret (confidential clients)")
	f.Var(&p.scopes, "scope", "OAuth scope to request (repeatable, or comma-separated)")
	f.IntVar(&p.port, "port", 0,
		"Local port for the OAuth callback (default: reuse the registered one, or pick a free one)")
	f.BoolVar(
		&p.noBrowser,
		"no-browser",
		false,
		"Print the authorization URL instead of opening it",
	)
	f.BoolVar(&p.force, "force", false,
		"Authorize even if the server accepts unauthenticated requests")
	f.DurationVar(&p.timeout, "timeout", 5*time.Minute, "How long to wait for authorization")
}

func (p *AuthCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		fmt.Fprint(os.Stderr, p.Usage())
		f.PrintDefaults()
		return subcommands.ExitUsageError
	}
	name := f.Arg(0)
	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	srv, err := remoteServer(env, name)
	if err != nil {
		return fail(err)
	}

	var scopes []string
	for _, s := range p.scopes {
		for part := range strings.SplitSeq(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				scopes = append(scopes, part)
			}
		}
	}
	headers := make(map[string]string, len(srv.Headers))
	for k, v := range srv.Headers {
		headers[k] = config.ExpandEnv(v)
	}
	opts := auth.Options{
		Remote:       remote.Config{URL: srv.URL, Transport: srv.Transport, Headers: headers},
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		Scopes:       scopes,
		Port:         p.port,
		Force:        p.force,
		Out:          os.Stderr,
		Timeout:      p.timeout,
	}
	if !p.noBrowser {
		opts.OpenBrowser = openBrowser
	}

	err = auth.Authorize(ctx, env.creds, opts)
	if errors.Is(err, auth.ErrNotRequired) {
		fmt.Fprintf(os.Stderr, "%s (%s) accepts unauthenticated requests; nothing to do "+
			"(use -force to authorize anyway).\n", name, srv.URL)
		return subcommands.ExitSuccess
	}
	if err != nil {
		return fail(err)
	}

	// Verify the stored credentials with a real connection.
	up, err := connect(ctx, env, name, false)
	if err != nil {
		return fail(
			fmt.Errorf("authorized, but connecting with the new credentials failed: %w", err),
		)
	}
	defer func() { _ = up.Close() }()
	fmt.Fprintf(os.Stderr, "Authorized. %s has %d tools.\n", name, len(up.Tools()))
	return subcommands.ExitSuccess
}

// LogoutCmd deletes stored credentials.
type LogoutCmd struct{}

func (*LogoutCmd) Name() string     { return "logout" }
func (*LogoutCmd) Synopsis() string { return "Delete stored credentials for a URL server." }
func (*LogoutCmd) Usage() string {
	return `logout <name>:
  Delete the OAuth client registration and tokens stored by "mcpproxy auth".
`
}

func (*LogoutCmd) SetFlags(*flag.FlagSet) {}

func (p *LogoutCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		fmt.Fprint(os.Stderr, p.Usage())
		return subcommands.ExitUsageError
	}
	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	srv, err := remoteServer(env, f.Arg(0))
	if err != nil {
		return fail(err)
	}
	deleted, err := env.creds.Delete(srv.URL)
	if err != nil {
		return fail(err)
	}
	if !deleted {
		fmt.Fprintf(os.Stderr, "no stored credentials for %s\n", f.Arg(0))
		return subcommands.ExitFailure
	}
	fmt.Fprintf(os.Stderr, "deleted credentials for %s\n", f.Arg(0))
	return subcommands.ExitSuccess
}
