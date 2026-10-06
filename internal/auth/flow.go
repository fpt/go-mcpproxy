package auth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// ErrNotRequired is returned by Authorize when the server accepted an
// unauthenticated connection.
var ErrNotRequired = errors.New("server does not require authorization")

// ClientName is the name mcpproxy registers with authorization servers.
const ClientName = "mcpproxy"

const callbackPath = "/callback"

// Options configures Authorize.
type Options struct {
	Remote remote.Config
	// ClientID and ClientSecret identify a pre-registered client. When empty,
	// a previously stored registration is reused or dynamic client
	// registration is performed.
	ClientID     string
	ClientSecret string
	Scopes       []string
	// Port of the local callback server; 0 reuses the stored redirect URI's
	// port, or picks a free one.
	Port int
	// Force runs the flow even if the server accepts unauthenticated access.
	Force bool
	// OpenBrowser opens the authorization URL; nil only prints it.
	OpenBrowser func(url string) error
	// Out receives progress messages.
	Out io.Writer
	// Timeout bounds the wait for the user to complete authorization.
	Timeout time.Duration
}

// Authorize runs the OAuth authorization code flow with PKCE for a remote
// MCP server and stores the resulting credentials.
func Authorize(ctx context.Context, store *Store, opts Options) error {
	serverURL := opts.Remote.URL
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}

	metadataURL, err := probe(ctx, opts.Remote)
	if err != nil {
		if !errors.Is(err, ErrNotRequired) || !opts.Force {
			return err
		}
	}

	prev, err := store.Load(serverURL)
	if err != nil {
		return err
	}

	ln, err := listenCallback(opts, prev)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, callbackPath)
	cfg := oauthConfig(opts, prev, redirectURI, metadataURL)

	h := transport.NewOAuthHandler(cfg)
	h.SetBaseURL(discoveryURL(serverURL))

	if cfg.ClientID == "" {
		fmt.Fprintln(opts.Out, "Registering mcpproxy with the authorization server...")
		if err := h.RegisterClient(ctx, ClientName); err != nil {
			return fmt.Errorf("dynamic client registration failed (pass -client-id if the "+
				"server needs a pre-registered client): %w", err)
		}
	}

	verifier, err := client.GenerateCodeVerifier()
	if err != nil {
		return err
	}
	state, err := client.GenerateState()
	if err != nil {
		return err
	}
	authURL, err := h.GetAuthorizationURL(ctx, state, client.GenerateCodeChallenge(verifier))
	if err != nil {
		return fmt.Errorf("build authorization URL: %w", err)
	}

	results := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(results),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	fmt.Fprintf(opts.Out, "Open this URL to authorize mcpproxy:\n\n  %s\n\n", authURL)
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(opts.Out, "(could not open a browser: %v)\n", err)
		}
	}
	fmt.Fprintln(opts.Out, "Waiting for authorization...")

	res, err := waitCallback(ctx, results, opts.Timeout)
	if err != nil {
		return err
	}
	if err := h.ProcessAuthorizationResponse(ctx, res.code, res.state, verifier); err != nil {
		return fmt.Errorf("exchange authorization code: %w", err)
	}

	token, err := cfg.TokenStore.GetToken(ctx)
	if err != nil {
		return fmt.Errorf("read issued token: %w", err)
	}
	return store.Save(&Credentials{
		ServerURL:    serverURL,
		ClientID:     h.GetClientID(),
		ClientSecret: h.GetClientSecret(),
		RedirectURI:  redirectURI,
		Scopes:       cfg.Scopes,
		Token:        token,
	})
}

// listenCallback opens the local callback listener, reusing the port of a
// stored dynamic registration so the registration stays valid.
func listenCallback(opts Options, prev *Credentials) (net.Listener, error) {
	port := opts.Port
	if port == 0 && opts.ClientID == "" && prev != nil {
		port = redirectPort(prev.RedirectURI)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil && opts.Port == 0 && port != 0 {
		// The stored port is taken; register again with a fresh one.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, fmt.Errorf("listen for OAuth callback: %w", err)
	}
	return ln, nil
}

// oauthConfig combines explicit options with a stored registration that is
// still valid for redirectURI.
func oauthConfig(
	opts Options,
	prev *Credentials,
	redirectURI, metadataURL string,
) transport.OAuthConfig {
	cfg := transport.OAuthConfig{
		ClientID:                     opts.ClientID,
		ClientSecret:                 opts.ClientSecret,
		RedirectURI:                  redirectURI,
		Scopes:                       opts.Scopes,
		TokenStore:                   transport.NewMemoryTokenStore(),
		PKCEEnabled:                  true,
		ProtectedResourceMetadataURL: metadataURL,
	}
	if cfg.ClientID == "" && prev != nil && prev.RedirectURI == redirectURI {
		cfg.ClientID, cfg.ClientSecret = prev.ClientID, prev.ClientSecret
		if len(cfg.Scopes) == 0 {
			cfg.Scopes = prev.Scopes
		}
	}
	return cfg
}

func waitCallback(
	ctx context.Context,
	results <-chan callbackResult,
	timeout time.Duration,
) (callbackResult, error) {
	select {
	case res := <-results:
		return res, res.err
	case <-time.After(timeout):
		return callbackResult{}, fmt.Errorf("timed out after %s waiting for authorization", timeout)
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	}
}

// probe connects without credentials. It returns ErrNotRequired if that
// works, or the protected resource metadata URL advertised in the 401.
func probe(ctx context.Context, cfg remote.Config) (string, error) {
	cfg.OAuth = nil
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c, err := remote.NewClient(ctx, cfg)
	if err == nil {
		defer func() { _ = c.Close() }()
		_, err = c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: ClientName},
		}})
	}
	switch {
	case err == nil:
		return "", ErrNotRequired
	case remote.IsAuthError(err):
		return client.GetResourceMetadataURL(err), nil
	default:
		return "", fmt.Errorf("probe %s: %w", cfg.URL, err)
	}
}

type callbackResult struct {
	code, state string
	err         error
}

func callbackHandler(results chan<- callbackResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res := callbackResult{code: q.Get("code"), state: q.Get("state")}
		msg := "Authorization complete. You can close this window."
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("authorization denied: %s %s", q.Get("error"),
				q.Get("error_description"))
			msg = "Authorization failed: " + res.err.Error()
		case res.code == "":
			res.err = errors.New("authorization callback carried no code")
			msg = "Authorization failed: no code received."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(
			w,
			"<!doctype html><title>mcpproxy</title><p>%s</p>",
			html.EscapeString(msg),
		)
		select {
		case results <- res:
		default:
		}
	})
	return mux
}

func discoveryURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func redirectPort(redirectURI string) int {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
