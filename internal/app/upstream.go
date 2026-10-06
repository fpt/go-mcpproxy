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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/mark3labs/mcp-go/client"
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
	// Dialer connects to the upstream.
	Dialer Dialer
	// Watch lists extra files whose modification triggers a restart, e.g. a
	// script run by an interpreter. The dialer's WatchPaths (the executable
	// of a stdio upstream) are always watched.
	Watch []string
	// Settle is how long watched files must stay unchanged before a restart.
	Settle time.Duration
	// StartTimeout bounds initialize + tools/list on the new process.
	StartTimeout time.Duration
	// Stderr receives a stdio upstream's stderr stream. Defaults to io.Discard.
	Stderr io.Writer
	// ToolPrefix is prepended to tool names in messages, matching the names
	// the client sees (e.g. "godev__").
	ToolPrefix string
	// ClientName and ClientVersion identify the proxy to the upstream.
	ClientName    string
	ClientVersion string
	Logger        *slog.Logger
}

// Status describes the current upstream state.
type Status struct {
	Target     string
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
	if opts.Dialer == nil {
		return nil, errors.New("upstream dialer is required")
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

	watch := slices.Clone(opts.Dialer.WatchPaths())
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

// Refresh restarts the upstream if a rebuild was detected or the process is
// not running, so that Tools reflects the build on disk.
func (u *Upstream) Refresh(ctx context.Context) error {
	if !u.stale() {
		return nil
	}
	return u.restart(ctx, false)
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
		Target:     u.opts.Dialer.String(),
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
		u.logger.InfoContext(
			ctx,
			"upstream changed on disk, restarting",
			"target",
			u.opts.Dialer.String(),
		)
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

	u.tail.Reset()
	c, tools, err := u.launch(ctx)
	if err != nil {
		err = u.explain(err)
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

	u.logger.InfoContext(ctx, "upstream started",
		"target", u.opts.Dialer.String(), "generation", gen, "tools", len(tools))
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

func (u *Upstream) launch(ctx context.Context) (*client.Client, []mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, u.opts.StartTimeout)
	defer cancel()

	target := u.opts.Dialer.String()
	c, err := u.opts.Dialer.Dial(ctx, io.MultiWriter(u.opts.Stderr, u.tail))
	if err != nil {
		return nil, nil, err
	}

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
		return nil, nil, fmt.Errorf("initialize %s: %w", target, err)
	}
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		closeQuietly(c)
		return nil, nil, fmt.Errorf("list tools of %s: %w", target, err)
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
	if err := u.relist(context.Background(), c); err != nil {
		u.logger.Warn("reload tools after list_changed failed", "error", err)
	}
}

// relist re-fetches the tool list from c if it is still the current client.
func (u *Upstream) relist(ctx context.Context, c *client.Client) error {
	u.restartMu.Lock()
	defer u.restartMu.Unlock()

	u.mu.RLock()
	current := u.client
	u.mu.RUnlock()
	if current != c {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, u.opts.StartTimeout)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return err
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
	return nil
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
	if err := u.Refresh(ctx); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(
			"mcpproxy: upstream %s could not be (re)started: %v", u.opts.Dialer.String(), err,
		)), nil
	}

	c, rejected := u.check(req)
	if rejected != nil && c != nil {
		// The cached tool list may be outdated, e.g. a remote server that
		// reloaded itself. Re-list once before rejecting; this costs nothing
		// on the happy path.
		if err := u.relist(ctx, c); err == nil {
			c, rejected = u.check(req)
		}
	}
	if rejected != nil {
		return rejected, nil
	}

	name := req.Params.Name
	res, err := c.CallTool(ctx, req)
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if remote.IsAuthError(err) {
		// Drop the connection so the next call reconnects with whatever
		// credentials "mcpproxy auth" has stored in the meantime.
		u.markCrashed(c, err)
		return mcp.NewToolResultError(fmt.Sprintf(
			"mcpproxy: call to upstream tool %q failed: %v", u.opts.ToolPrefix+name, u.explain(err),
		)), nil
	}
	msg := fmt.Sprintf("mcpproxy: call to upstream tool %q failed: %v", u.opts.ToolPrefix+name, err)
	if !u.alive(c) {
		u.markCrashed(c, err)
		if tail := u.tail.String(); tail != "" {
			msg += "\n\nThe upstream process appears to have exited. Recent stderr:\n" + tail
		}
	}
	return mcp.NewToolResultError(msg), nil
}

// check returns the current client and, if the call cannot be forwarded, the
// error result explaining why.
func (u *Upstream) check(req mcp.CallToolRequest) (*client.Client, *mcp.CallToolResult) {
	name := req.Params.Name
	u.mu.RLock()
	c := u.client
	tool, found := findTool(u.tools, name)
	schema := u.schemas[name]
	names := toolNames(u.tools)
	for i := range names {
		names[i] = u.opts.ToolPrefix + names[i]
	}
	lastErr := u.lastErr
	u.mu.RUnlock()

	if c == nil {
		if lastErr == nil {
			lastErr = ErrNotRunning
		}
		return nil, mcp.NewToolResultError(fmt.Sprintf("mcpproxy: %v", lastErr))
	}
	if !found {
		return c, mcp.NewToolResultError(fmt.Sprintf(
			"mcpproxy: tool %q does not exist on the upstream server (its tools may have "+
				"changed since you listed them). Available tools: %s",
			u.opts.ToolPrefix+name, strings.Join(names, ", "),
		))
	}
	if schema != nil {
		if err := validateArguments(schema, req); err != nil {
			return c, invalidArgumentsResult(u.opts.ToolPrefix+tool.Name, tool, err)
		}
	}
	return c, nil
}

// explain appends a hint on how to authorize to authorization errors.
func (u *Upstream) explain(err error) error {
	if hint := u.opts.Dialer.AuthHint(); hint != "" && remote.IsAuthError(err) {
		return fmt.Errorf("%w\n\n%s", err, hint)
	}
	return err
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

func invalidArgumentsResult(name string, tool mcp.Tool, err error) *mcp.CallToolResult {
	schema, _ := inputSchemaJSON(tool)
	var pretty bytes.Buffer
	if json.Indent(&pretty, schema, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(schema)
	}
	return mcp.NewToolResultError(fmt.Sprintf(
		"mcpproxy: arguments for %q do not match the tool's current input schema "+
			"(it may have changed since you listed it): %v\n\nCurrent input schema:\n%s",
		name, err, pretty.String(),
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
