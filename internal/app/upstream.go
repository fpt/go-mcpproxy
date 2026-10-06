// Package app implements the upstream MCP server supervisor used by mcpproxy.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	stderrTailBytes = 4096
	maxSettleWait   = 30 * time.Second
	pingTimeout     = 2 * time.Second
)

// ErrNotRunning is reported when the upstream process is not available.
var ErrNotRunning = errors.New("upstream server is not running")

// Options configures an Upstream.
type Options struct {
	// Command is the upstream executable, as given on the command line.
	Command string
	// Args are passed to the upstream executable.
	Args []string
	// Env is appended to the proxy's own environment.
	Env []string
	// Watch lists extra files whose modification triggers a restart, e.g. a
	// script run by an interpreter. The executable itself is always watched.
	Watch []string
	// Settle is how long watched files must stay unchanged before a restart.
	Settle time.Duration
	// StartTimeout bounds initialize + tools/list on the new process.
	StartTimeout time.Duration
	// Stderr receives the upstream's stderr stream. Defaults to io.Discard.
	Stderr io.Writer
	// Resolve maps Command to the path to execute, enforcing the allowlist.
	// It runs before every (re)start so a retargeted symlink is re-checked.
	Resolve func(command string) (string, error)
	// ClientName and ClientVersion identify the proxy to the upstream.
	ClientName    string
	ClientVersion string
	Logger        *slog.Logger
}

// Status describes the current upstream state.
type Status struct {
	Command    string
	Args       []string
	Running    bool
	Generation int
	StartedAt  time.Time
	Tools      []string
	LastError  string
	Stderr     string
}

// Upstream supervises one upstream MCP server process. It restarts the
// process when the executable (or another watched file) changes and
// validates tool calls against the schemas of the currently running build.
//
// Upstream is safe for concurrent use.
type Upstream struct {
	opts   Options
	watch  []string
	tail   *tailBuffer
	logger *slog.Logger

	// restartMu serializes restarts; mu guards the fields below it.
	restartMu sync.Mutex
	mu        sync.RWMutex
	client    *client.Client
	tools     []mcp.Tool
	schemas   map[string]*jsonschema.Schema
	fp        fingerprint
	lastErr   error
	gen       int
	startedAt time.Time

	onToolsChanged func([]mcp.Tool)
}

// New creates an Upstream. It does not start the process; call Start.
func New(opts Options) (*Upstream, error) {
	if opts.Command == "" {
		return nil, errors.New("upstream command is required")
	}
	if opts.Resolve == nil {
		return nil, errors.New("upstream resolver is required")
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.ClientName == "" {
		opts.ClientName = "mcpproxy"
	}

	exe, err := watchPathForCommand(opts.Command)
	if err != nil {
		return nil, err
	}
	watch := []string{exe}
	for _, w := range opts.Watch {
		abs, err := filepath.Abs(w)
		if err != nil {
			return nil, fmt.Errorf("watch path %q: %w", w, err)
		}
		watch = append(watch, abs)
	}

	return &Upstream{
		opts:   opts,
		watch:  watch,
		tail:   newTailBuffer(stderrTailBytes),
		logger: opts.Logger,
	}, nil
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

// OnToolsChanged registers fn to be called with the new tool list whenever
// a restart or upstream notification changes the set of tools.
// It must be called before Start.
func (u *Upstream) OnToolsChanged(fn func([]mcp.Tool)) {
	u.onToolsChanged = fn
}

// Start launches the upstream process. A failure is recorded and returned,
// but the Upstream remains usable: the next tool call or detected rebuild
// retries.
func (u *Upstream) Start(ctx context.Context) error {
	return u.restart(ctx, true)
}

// Restart unconditionally restarts the upstream process.
func (u *Upstream) Restart(ctx context.Context) error {
	return u.restart(ctx, true)
}

// Close stops the upstream process.
func (u *Upstream) Close() error {
	u.restartMu.Lock()
	defer u.restartMu.Unlock()
	u.mu.Lock()
	c := u.client
	u.client = nil
	u.lastErr = ErrNotRunning
	u.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// Tools returns the tools of the most recent successful start.
func (u *Upstream) Tools() []mcp.Tool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return slices.Clone(u.tools)
}

// Status reports the current upstream state.
func (u *Upstream) Status() Status {
	u.mu.RLock()
	defer u.mu.RUnlock()
	st := Status{
		Command:    u.opts.Command,
		Args:       u.opts.Args,
		Running:    u.client != nil,
		Generation: u.gen,
		StartedAt:  u.startedAt,
		Stderr:     u.tail.String(),
	}
	for _, t := range u.tools {
		st.Tools = append(st.Tools, t.Name)
	}
	if u.lastErr != nil {
		st.LastError = u.lastErr.Error()
	}
	return st
}

// Watch polls the watched files every interval and restarts the upstream
// as soon as a rebuild is detected, so that the tool list (and the
// list_changed notification) is up to date before the next call.
// It returns when ctx is done.
func (u *Upstream) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !u.changed() {
			continue
		}
		u.logger.InfoContext(ctx, "upstream changed on disk, restarting", "command", u.opts.Command)
		if err := u.restart(ctx, false); err != nil {
			u.logger.WarnContext(ctx, "upstream restart failed", "error", err)
		}
	}
}

// changed reports whether any watched file differs from the running build.
func (u *Upstream) changed() bool {
	cur := takeFingerprint(u.watch)
	u.mu.RLock()
	defer u.mu.RUnlock()
	return !cur.Equal(u.fp)
}

// stale reports whether a call must restart the upstream first: either the
// files changed or the process is not running (failed start or crash).
func (u *Upstream) stale() bool {
	u.mu.RLock()
	running := u.client != nil
	u.mu.RUnlock()
	return !running || u.changed()
}

func (u *Upstream) restart(ctx context.Context, force bool) error {
	u.restartMu.Lock()
	defer u.restartMu.Unlock()

	if !force && !u.stale() {
		// Another caller restarted while we waited for the lock.
		return nil
	}

	fp := waitSettled(ctx, u.watch, u.opts.Settle, maxSettleWait)

	path, err := u.opts.Resolve(u.opts.Command)
	if err != nil {
		u.fail(fp, err)
		return err
	}

	u.tail.Reset()
	c, tools, err := u.launch(ctx, path)
	if err != nil {
		if tail := u.tail.String(); tail != "" {
			err = fmt.Errorf("%w\n\nupstream stderr:\n%s", err, tail)
		}
		u.fail(fp, err)
		return err
	}

	schemas := make(map[string]*jsonschema.Schema, len(tools))
	for _, t := range tools {
		s, err := compileInputSchema(t)
		if err != nil {
			u.logger.WarnContext(ctx, "input schema not compilable, skipping validation",
				"tool", t.Name, "error", err)
		}
		schemas[t.Name] = s
	}

	u.mu.Lock()
	old := u.client
	changed := !sameTools(u.tools, tools)
	u.client = c
	u.tools = tools
	u.schemas = schemas
	u.fp = fp
	u.lastErr = nil
	u.gen++
	u.startedAt = time.Now()
	gen := u.gen
	u.mu.Unlock()

	u.logger.InfoContext(
		ctx,
		"upstream started",
		"path",
		path,
		"generation",
		gen,
		"tools",
		len(tools),
	)
	if old != nil {
		go closeQuietly(old)
	}
	if changed && u.onToolsChanged != nil {
		u.onToolsChanged(slices.Clone(tools))
	}
	return nil
}

// fail records a failed (re)start. The previous process is stopped because
// it no longer reflects the code on disk; the previous tool list is kept so
// the agent keeps seeing tools and gets a descriptive error when calling them.
func (u *Upstream) fail(fp fingerprint, err error) {
	u.mu.Lock()
	old := u.client
	u.client = nil
	u.fp = fp
	u.lastErr = err
	u.mu.Unlock()
	if old != nil {
		go closeQuietly(old)
	}
}

func (u *Upstream) launch(ctx context.Context, path string) (*client.Client, []mcp.Tool, error) {
	stderr := io.MultiWriter(u.opts.Stderr, u.tail)
	c, err := client.NewStdioMCPClientWithOptions(path, u.opts.Env, u.opts.Args,
		transport.WithCommandStderrWriter(stderr))
	if err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", path, err)
	}

	ctx, cancel := context.WithTimeout(ctx, u.opts.StartTimeout)
	defer cancel()

	_, err = c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    u.opts.ClientName,
				Version: u.opts.ClientVersion,
			},
		},
	})
	if err != nil {
		closeQuietly(c)
		return nil, nil, fmt.Errorf("initialize %s: %w", path, err)
	}
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		closeQuietly(c)
		return nil, nil, fmt.Errorf("list tools of %s: %w", path, err)
	}

	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == string(mcp.MethodNotificationToolsListChanged) {
			go u.reloadTools(c)
		}
	})
	return c, res.Tools, nil
}

// reloadTools refreshes the tool list after the upstream itself announced
// a change, without restarting the process.
func (u *Upstream) reloadTools(c *client.Client) {
	u.restartMu.Lock()
	defer u.restartMu.Unlock()

	u.mu.RLock()
	current := u.client
	u.mu.RUnlock()
	if current != c {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), u.opts.StartTimeout)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		u.logger.Warn("reload tools after list_changed failed", "error", err)
		return
	}
	schemas := make(map[string]*jsonschema.Schema, len(res.Tools))
	for _, t := range res.Tools {
		schemas[t.Name], _ = compileInputSchema(t)
	}

	u.mu.Lock()
	changed := !sameTools(u.tools, res.Tools)
	u.tools = res.Tools
	u.schemas = schemas
	u.mu.Unlock()
	if changed && u.onToolsChanged != nil {
		u.onToolsChanged(slices.Clone(res.Tools))
	}
}

// CallTool forwards a tool call to the upstream. Before forwarding it
// restarts the upstream if a rebuild was detected and validates the
// arguments against the running build's input schema, so that a call
// shaped for an older build fails fast with the current schema attached.
//
// Failures are reported as tool error results so the agent can read them.
func (u *Upstream) CallTool(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if u.stale() {
		if err := u.restart(ctx, false); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf(
				"mcpproxy: upstream %s could not be (re)started: %v", u.opts.Command, err,
			)), nil
		}
	}

	name := req.Params.Name
	u.mu.RLock()
	c := u.client
	tool, found := findTool(u.tools, name)
	schema := u.schemas[name]
	names := toolNames(u.tools)
	lastErr := u.lastErr
	u.mu.RUnlock()

	if c == nil {
		if lastErr == nil {
			lastErr = ErrNotRunning
		}
		return mcp.NewToolResultError(fmt.Sprintf("mcpproxy: %v", lastErr)), nil
	}
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf(
			"mcpproxy: tool %q does not exist in the current build of the upstream server. "+
				"Available tools: %s", name, strings.Join(names, ", "),
		)), nil
	}
	if schema != nil {
		if err := validateArguments(schema, req); err != nil {
			return invalidArgumentsResult(tool, err), nil
		}
	}

	res, err := c.CallTool(ctx, req)
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	msg := fmt.Sprintf("mcpproxy: call to upstream tool %q failed: %v", name, err)
	if !u.alive(c) {
		u.markCrashed(c, err)
		if tail := u.tail.String(); tail != "" {
			msg += "\n\nThe upstream process appears to have exited. Recent stderr:\n" + tail
		}
	}
	return mcp.NewToolResultError(msg), nil
}

func (u *Upstream) alive(c *client.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	// The legacy protocol is negotiated in launch, where ping still exists.
	err := c.Ping(ctx) //nolint:staticcheck
	return err == nil
}

func (u *Upstream) markCrashed(c *client.Client, err error) {
	u.mu.Lock()
	if u.client != c {
		u.mu.Unlock()
		return
	}
	u.client = nil
	u.lastErr = fmt.Errorf("%w: crashed: %w", ErrNotRunning, err)
	u.mu.Unlock()
	go closeQuietly(c)
}

func invalidArgumentsResult(tool mcp.Tool, err error) *mcp.CallToolResult {
	schema, _ := inputSchemaJSON(tool)
	var pretty bytes.Buffer
	if json.Indent(&pretty, schema, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(schema)
	}
	return mcp.NewToolResultError(fmt.Sprintf(
		"mcpproxy: arguments for %q do not match the input schema of the currently running build "+
			"(the tool may have changed since you last listed it): %v\n\nCurrent input schema:\n%s",
		tool.Name, err, pretty.String(),
	))
}

func findTool(tools []mcp.Tool, name string) (mcp.Tool, bool) {
	i := slices.IndexFunc(tools, func(t mcp.Tool) bool { return t.Name == name })
	if i < 0 {
		return mcp.Tool{}, false
	}
	return tools[i], true
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func sameTools(a, b []mcp.Tool) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

func closeQuietly(c *client.Client) {
	_ = c.Close()
}
