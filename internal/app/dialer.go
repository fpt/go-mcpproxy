package app

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
)

// Dialer connects to an upstream MCP server.
type Dialer interface {
	// Dial creates and starts a client; the caller initializes it. stderr
	// receives the output of a locally launched process.
	Dial(ctx context.Context, stderr io.Writer) (*client.Client, error)
	// WatchPaths lists local files whose modification requires reconnecting.
	WatchPaths() []string
	// String identifies the upstream in messages.
	String() string
	// AuthHint explains how to fix an authorization failure, or "" if the
	// upstream does not use authorization.
	AuthHint() string
}

// StdioDialer launches a local executable speaking MCP over stdio.
type StdioDialer struct {
	command string
	args    []string
	env     []string
	exe     string
	resolve func(command string) (string, error)
}

// NewStdioDialer returns a dialer for command. resolve maps the command to
// the path to execute and enforces the allowlist; it runs on every dial so
// that a retargeted symlink is re-checked.
func NewStdioDialer(
	command string,
	args, env []string,
	resolve func(string) (string, error),
) (*StdioDialer, error) {
	exe, err := watchPathForCommand(command)
	if err != nil {
		return nil, err
	}
	return &StdioDialer{command: command, args: args, env: env, exe: exe, resolve: resolve}, nil
}

// watchPathForCommand returns the absolute path to stat for change
// detection. Symlinks are deliberately not resolved here so that a
// retargeted symlink is noticed (os.Stat follows it).
func watchPathForCommand(command string) (string, error) {
	if strings.ContainsRune(command, filepath.Separator) {
		return filepath.Abs(command)
	}
	p, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("locate %q: %w", command, err)
	}
	return filepath.Abs(p)
}

// Dial implements Dialer.
func (d *StdioDialer) Dial(_ context.Context, stderr io.Writer) (*client.Client, error) {
	path, err := d.resolve(d.command)
	if err != nil {
		return nil, err
	}
	c, err := client.NewStdioMCPClientWithOptions(path, d.env, d.args,
		transport.WithCommandStderrWriter(stderr))
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}
	return c, nil
}

// WatchPaths implements Dialer.
func (d *StdioDialer) WatchPaths() []string { return []string{d.exe} }

// String implements Dialer.
func (d *StdioDialer) String() string {
	return strings.TrimSpace(d.command + " " + strings.Join(d.args, " "))
}

// AuthHint implements Dialer.
func (d *StdioDialer) AuthHint() string { return "" }

// RemoteDialer connects to an HTTP or SSE MCP server.
type RemoteDialer struct {
	cfg   remote.Config
	check func(url string) error
	oauth func(url string) (*transport.OAuthConfig, error)
}

// NewRemoteDialer returns a dialer for cfg. check enforces the allowlist and
// oauth loads stored OAuth credentials (nil config for none); both run on
// every dial, so credentials stored by "mcpproxy auth" while the proxy is
// running are picked up on the next reconnect.
func NewRemoteDialer(
	cfg remote.Config,
	check func(string) error,
	oauth func(string) (*transport.OAuthConfig, error),
) *RemoteDialer {
	return &RemoteDialer{cfg: cfg, check: check, oauth: oauth}
}

// Dial implements Dialer.
func (d *RemoteDialer) Dial(ctx context.Context, _ io.Writer) (*client.Client, error) {
	if err := d.check(d.cfg.URL); err != nil {
		return nil, err
	}
	cfg := d.cfg
	if d.oauth != nil {
		o, err := d.oauth(cfg.URL)
		if err != nil {
			return nil, err
		}
		cfg.OAuth = o
	}
	return remote.NewClient(ctx, cfg)
}

// WatchPaths implements Dialer.
func (d *RemoteDialer) WatchPaths() []string { return nil }

// String implements Dialer.
func (d *RemoteDialer) String() string { return d.cfg.URL }

// AuthHint implements Dialer.
func (d *RemoteDialer) AuthHint() string {
	cmd := "mcpproxy auth " + d.cfg.URL
	if d.cfg.Transport != remote.TransportAuto {
		cmd = "mcpproxy auth -transport " + d.cfg.Transport + " " + d.cfg.URL
	}
	return fmt.Sprintf("The server requires authorization. Run `%s` in a terminal "+
		"(ask the user to do so if you are an AI agent), then retry.", cmd)
}
