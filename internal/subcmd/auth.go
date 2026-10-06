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
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/google/subcommands"
)

// openBrowser opens the authorization URL; tests replace it.
var openBrowser = auth.OpenBrowser

// AuthCmd authorizes mcpproxy to access a remote MCP server via OAuth.
type AuthCmd struct {
	target       targetFlags
	clientID     string
	clientSecret string
	scopes       stringList
	port         int
	noBrowser    bool
	force        bool
	timeout      time.Duration
}

func (*AuthCmd) Name() string     { return "auth" }
func (*AuthCmd) Synopsis() string { return "Authorize access to a remote MCP server (OAuth)." }
func (*AuthCmd) Usage() string {
	return `auth [flags] <url>:
  Run the OAuth authorization flow for a streamable HTTP or SSE MCP server in
  the browser and store the credentials, which "serve", "tools" and "call"
  then use (refreshing the token when needed). A proxy that is already
  running picks them up on its next reconnect.

  The client is registered dynamically unless -client-id is given. For a
  server that takes a static token instead, pass it with -header to serve,
  tools and call, e.g. -header "Authorization: Bearer $TOKEN".

`
}

func (p *AuthCmd) SetFlags(f *flag.FlagSet) {
	p.target.setFlags(f)
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
	if f.NArg() != 1 || !remote.IsURL(f.Arg(0)) {
		fmt.Fprint(os.Stderr, p.Usage())
		f.PrintDefaults()
		return subcommands.ExitUsageError
	}
	serverURL := f.Arg(0)

	env, err := loadEnvironment()
	if err != nil {
		return fail(err)
	}
	if err := env.allow.CheckURL(serverURL); err != nil {
		return fail(env.rejected(err))
	}
	cfg, err := p.target.remoteConfig(serverURL)
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
	opts := auth.Options{
		Remote:       cfg,
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
		fmt.Fprintf(
			os.Stderr,
			"%s accepts unauthenticated requests; nothing to do (use -force to authorize anyway).\n",
			serverURL,
		)
		return subcommands.ExitSuccess
	}
	if err != nil {
		return fail(err)
	}

	// Verify the stored credentials with a real connection.
	dialer, err := p.target.dialer(env, []string{serverURL})
	if err != nil {
		return fail(err)
	}
	up, err := connect(ctx, dialer, false)
	if err != nil {
		return fail(
			fmt.Errorf("authorized, but connecting with the new credentials failed: %w", err),
		)
	}
	defer func() { _ = up.Close() }()
	fmt.Fprintf(os.Stderr, "Authorized. %s has %d tools.\n", serverURL, len(up.Tools()))
	return subcommands.ExitSuccess
}

// LogoutCmd deletes stored credentials.
type LogoutCmd struct{}

func (*LogoutCmd) Name() string     { return "logout" }
func (*LogoutCmd) Synopsis() string { return "Delete stored credentials for a remote MCP server." }
func (*LogoutCmd) Usage() string {
	return `logout <url>:
  Delete the OAuth client registration and tokens stored by "mcpproxy auth".
`
}

func (*LogoutCmd) SetFlags(*flag.FlagSet) {}

func (p *LogoutCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 || !remote.IsURL(f.Arg(0)) {
		fmt.Fprint(os.Stderr, p.Usage())
		return subcommands.ExitUsageError
	}
	env, err := loadEnvironment()
	if err != nil {
		return fail(err)
	}
	deleted, err := env.creds.Delete(f.Arg(0))
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

func fail(err error) subcommands.ExitStatus {
	fmt.Fprintln(os.Stderr, "mcpproxy:", err)
	return subcommands.ExitFailure
}
