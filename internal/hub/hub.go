// Package hub runs the set of backend MCP servers described by the config
// file and keeps it in sync with the file as it changes.
package hub

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/auth"
	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Options configures a Hub and the upstreams it creates.
type Options struct {
	// Poll is the interval for watching backend executables; 0 disables
	// background watching (changes are still detected on each call).
	Poll time.Duration
	// Settle is how long watched files must be unchanged before a restart.
	Settle time.Duration
	// StartTimeout bounds connecting, initializing and listing tools.
	StartTimeout time.Duration
	// Stderr receives the stderr of stdio backends.
	Stderr io.Writer
	// Creds holds OAuth credentials for remote backends; nil disables OAuth.
	Creds         *auth.Store
	ClientVersion string
	Logger        *slog.Logger
}

// Backend is a snapshot of one configured server.
type Backend struct {
	Name   string
	Server config.Server
	// Upstream is nil if the server could not be set up; see Err.
	Upstream *app.Upstream
	Err      error
}

type backend struct {
	Backend
	// ctx lives as long as the backend; cancel stops its watcher.
	ctx    context.Context
	cancel context.CancelFunc
}

// Hub owns one app.Upstream per configured server. It is safe for
// concurrent use.
type Hub struct {
	opts Options

	applyMu   sync.Mutex
	mu        sync.RWMutex
	backends  map[string]*backend
	configErr error
	onChange  func()
}

// New returns an empty hub; call Apply to start servers.
func New(opts Options) *Hub {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	return &Hub{opts: opts, backends: map[string]*backend{}}
}

// OnChange registers fn to be called whenever the set of backends or the
// tools of any backend change. It must be called before Apply.
func (h *Hub) OnChange(fn func()) {
	h.onChange = fn
}

func (h *Hub) notify() {
	if h.onChange != nil {
		h.onChange()
	}
}

// Backends returns the configured backends sorted by name.
func (h *Hub) Backends() []Backend {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Backend, 0, len(h.backends))
	for _, name := range slices.Sorted(maps.Keys(h.backends)) {
		out = append(out, h.backends[name].Backend)
	}
	return out
}

// Get returns the backend with the given name.
func (h *Hub) Get(name string) (Backend, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	b, ok := h.backends[name]
	if !ok {
		return Backend{}, false
	}
	return b.Backend, true
}

// ConfigError returns the error of the most recent failed config load.
func (h *Hub) ConfigError() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.configErr
}

// SetConfigError records that the config could not be loaded; the current
// backends keep running.
func (h *Hub) SetConfigError(err error) {
	h.mu.Lock()
	h.configErr = err
	h.mu.Unlock()
}

// Apply makes the running backends match cfg: removed or changed servers
// are stopped, and new or changed servers are started in parallel. It
// returns once every new server has started or failed to start.
func (h *Hub) Apply(ctx context.Context, cfg *config.Config) {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()

	h.mu.Lock()
	h.configErr = nil
	var stopped, started []*backend
	for name, b := range h.backends {
		if srv, ok := cfg.Servers[name]; !ok || !srv.Equal(b.Server) {
			delete(h.backends, name)
			stopped = append(stopped, b)
		}
	}
	for _, name := range cfg.Names() {
		if _, ok := h.backends[name]; ok {
			continue
		}
		b := h.newBackend(name, cfg.Servers[name])
		h.backends[name] = b
		started = append(started, b)
	}
	h.mu.Unlock()

	for _, b := range stopped {
		h.opts.Logger.InfoContext(ctx, "stopping backend", "server", b.Name)
		b.cancel()
		if b.Upstream != nil {
			go func() { _ = b.Upstream.Close() }()
		}
	}

	var wg sync.WaitGroup
	for _, b := range started {
		if b.Upstream == nil {
			h.opts.Logger.WarnContext(ctx, "backend not usable", "server", b.Name, "error", b.Err)
			continue
		}
		wg.Go(func() {
			if err := b.Upstream.Start(ctx); err != nil {
				h.opts.Logger.WarnContext(ctx, "backend start failed",
					"server", b.Name, "error", err)
			}
		})
	}
	wg.Wait()

	for _, b := range started {
		if b.Upstream != nil && h.opts.Poll > 0 {
			go b.Upstream.Watch(b.ctx, h.opts.Poll)
		}
	}
	if len(stopped) > 0 || len(started) > 0 {
		h.notify()
	}
}

func (h *Hub) newBackend(name string, srv config.Server) *backend {
	ctx, cancel := context.WithCancel(context.Background())
	b := &backend{Backend: Backend{Name: name, Server: srv}, ctx: ctx, cancel: cancel}
	up, err := NewUpstream(name, srv, h.opts)
	if err != nil {
		b.Err = err
		return b
	}
	up.OnToolsChanged(func([]mcp.Tool) { h.notify() })
	b.Upstream = up
	return b
}

// Close stops every backend.
func (h *Hub) Close() {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()
	h.mu.Lock()
	backends := h.backends
	h.backends = map[string]*backend{}
	h.mu.Unlock()
	for _, b := range backends {
		b.cancel()
		if b.Upstream != nil {
			_ = b.Upstream.Close()
		}
	}
}

// WatchConfig polls the config file and applies it whenever it changes.
// An invalid file is reported through ConfigError and leaves the running
// backends untouched. It returns when ctx is done.
func (h *Hub) WatchConfig(ctx context.Context, path string, interval time.Duration) {
	last := stat(path)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := stat(path)
		if cur == last {
			continue
		}
		last = cur
		cfg, err := config.Load(path)
		if err != nil {
			h.opts.Logger.ErrorContext(ctx, "config reload failed; keeping current servers",
				"error", err)
			h.SetConfigError(err)
			h.notify()
			continue
		}
		h.opts.Logger.InfoContext(ctx, "config changed, reloading", "servers", cfg.Names())
		h.Apply(ctx, cfg)
	}
}

type fileStamp struct {
	exists  bool
	size    int64
	modTime time.Time
}

func stat(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, size: info.Size(), modTime: info.ModTime()}
}

// NewUpstream creates (but does not start) the upstream for a configured
// server. Tool names in its messages carry the "<name>__" prefix.
func NewUpstream(name string, srv config.Server, opts Options) (*app.Upstream, error) {
	if err := srv.Validate(); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	var dialer app.Dialer
	if srv.IsRemote() {
		headers := make(map[string]string, len(srv.Headers))
		for k, v := range srv.Headers {
			headers[k] = config.ExpandEnv(v)
		}
		var oauth func(string) (*transport.OAuthConfig, error)
		if opts.Creds != nil {
			oauth = opts.Creds.OAuthConfig
		}
		d := app.NewRemoteDialer(remote.Config{
			URL:       srv.URL,
			Transport: srv.Transport,
			Headers:   headers,
		}, nil, oauth)
		d.SetAuthCommand("mcpproxy auth " + name)
		dialer = d
	} else {
		env := make([]string, 0, len(srv.Env))
		for _, k := range slices.Sorted(maps.Keys(srv.Env)) {
			env = append(env, k+"="+config.ExpandEnv(srv.Env[k]))
		}
		d, err := app.NewStdioDialer(srv.Command, srv.Args, env, nil)
		if err != nil {
			return nil, err
		}
		dialer = d
	}
	return app.New(app.Options{
		Dialer:        dialer,
		Watch:         srv.Watch,
		Settle:        opts.Settle,
		StartTimeout:  opts.StartTimeout,
		Stderr:        opts.Stderr,
		ToolPrefix:    name + config.ToolSeparator,
		ClientVersion: opts.ClientVersion,
		Logger:        opts.Logger.With("server", name),
	})
}
