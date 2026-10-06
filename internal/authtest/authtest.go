// Package authtest provides an in-process MCP server protected by a fake
// OAuth 2.1 authorization server (dynamic registration, PKCE, refresh
// tokens) for tests.
package authtest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Server is a protected MCP server. Its tool "whoami" returns the access
// token used for the call.
type Server struct {
	*httptest.Server
	// TokenTTL is the lifetime of issued access tokens (default 1h).
	TokenTTL time.Duration
	// RequireAuth enables bearer authentication on the MCP endpoints.
	RequireAuth bool

	mu            sync.Mutex
	clients       map[string]string // client_id -> redirect_uri
	codes         map[string]codeGrant
	accessTokens  map[string]time.Time
	refreshTokens map[string]bool
	seq           int
	Refreshes     int
}

type codeGrant struct {
	clientID, redirectURI, challenge string
}

// NewServer starts a protected server. MCP is served at /mcp (streamable
// HTTP) and /sse (SSE).
func NewServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		TokenTTL:      time.Hour,
		RequireAuth:   true,
		clients:       map[string]string{},
		codes:         map[string]codeGrant{},
		accessTokens:  map[string]time.Time{},
		refreshTokens: map[string]bool{},
	}

	mcpServer := server.NewMCPServer("protected", "1.0")
	mcpServer.AddTool(mcp.NewTool("whoami", mcp.WithString("greeting")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(req.GetString("greeting", "hello") + " " +
				tokenFromContext(ctx)), nil
		})

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.protectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/", s.protectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("/register", s.register)
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/token", s.token)

	s.Server = httptest.NewUnstartedServer(mux)
	s.Start()
	t.Cleanup(s.Close)

	streamable := server.NewStreamableHTTPServer(mcpServer,
		server.WithHTTPContextFunc(withToken))
	sse := server.NewSSEServer(mcpServer, server.WithBaseURL(s.URL),
		server.WithSSEContextFunc(withToken))
	mux.Handle("/mcp", s.protect(streamable))
	mux.Handle("/sse", s.protect(sse.SSEHandler()))
	mux.Handle("/message", s.protect(sse.MessageHandler()))
	return s
}

type tokenKey struct{}

func withToken(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(
		ctx,
		tokenKey{},
		strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
	)
}

func tokenFromContext(ctx context.Context) string {
	s, _ := ctx.Value(tokenKey{}).(string)
	return s
}

// MCPURL is the streamable HTTP endpoint.
func (s *Server) MCPURL() string { return s.URL + "/mcp" }

// SSEURL is the SSE endpoint.
func (s *Server) SSEURL() string { return s.URL + "/sse" }

// Revoke invalidates all access and refresh tokens.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessTokens = map[string]time.Time{}
	s.refreshTokens = map[string]bool{}
}

func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.RequireAuth && !s.validAccessToken(r.Header.Get("Authorization")) {
			resource := r.URL.Path
			if resource == "/message" {
				resource = "/sse"
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer resource_metadata="%s/.well-known/oauth-protected-resource%s"`,
				s.URL, resource,
			))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validAccessToken(header string) bool {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.accessTokens[tok]
	return ok && time.Now().Before(exp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// protectedResource serves RFC 9728 metadata. The resource is the MCP
// endpoint whose path follows the well-known prefix.
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource")
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.URL + path,
		"authorization_servers": []string{s.URL},
	})
}

func (s *Server) authServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                           s.URL,
		"authorization_endpoint":           s.URL + "/authorize",
		"token_endpoint":                   s.URL + "/token",
		"registration_endpoint":            s.URL + "/register",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.RedirectURIs) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("client-%d", s.seq)
	s.clients[id] = req.RedirectURIs[0]
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":     id,
		"redirect_uris": req.RedirectURIs,
	})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	registered, ok := s.clients[q.Get("client_id")]
	s.mu.Unlock()
	if !ok || registered != q.Get("redirect_uri") || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.seq++
	code := fmt.Sprintf("code-%d", s.seq)
	s.codes[code] = codeGrant{q.Get("client_id"), q.Get("redirect_uri"), q.Get("code_challenge")}
	s.mu.Unlock()
	http.Redirect(w, r, fmt.Sprintf("%s?code=%s&state=%s", q.Get("redirect_uri"), code,
		q.Get("state")), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		g, ok := s.codes[r.Form.Get("code")]
		delete(s.codes, r.Form.Get("code"))
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || g.clientID != r.Form.Get("client_id") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
	case "refresh_token":
		if !s.refreshTokens[r.Form.Get("refresh_token")] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		delete(s.refreshTokens, r.Form.Get("refresh_token"))
		s.Refreshes++
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	s.seq++
	access := fmt.Sprintf("access-%d", s.seq)
	refresh := fmt.Sprintf("refresh-%d", s.seq)
	s.accessTokens[access] = time.Now().Add(s.TokenTTL)
	s.refreshTokens[refresh] = true
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "bearer",
		"expires_in":    int(s.TokenTTL.Seconds()),
		"refresh_token": refresh,
	})
}

// Browser simulates a user approving the request: it follows the
// authorization URL's redirect to the local callback.
func Browser(authURL string) error {
	go func() {
		resp, err := http.Get(authURL) //nolint:gosec,noctx // test helper
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	return nil
}
