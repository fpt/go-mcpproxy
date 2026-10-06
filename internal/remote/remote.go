// Package remote creates MCP clients for HTTP-based (streamable HTTP or
// SSE) MCP servers.
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
)

// Transport names.
const (
	TransportAuto = ""
	TransportHTTP = "http"
	TransportSSE  = "sse"
)

// IsURL reports whether s looks like an http(s) URL rather than a command.
func IsURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ParseURL validates an MCP server URL.
func ParseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse server URL %q: %w", s, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("server URL %q must be an absolute http(s) URL", s)
	}
	return u, nil
}

// ResolveTransport picks the transport for serverURL: an explicit choice
// wins; otherwise a path ending in "/sse" means SSE and anything else
// streamable HTTP.
func ResolveTransport(serverURL, choice string) (string, error) {
	switch choice {
	case TransportHTTP, TransportSSE:
		return choice, nil
	case TransportAuto:
		u, err := ParseURL(serverURL)
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/sse") {
			return TransportSSE, nil
		}
		return TransportHTTP, nil
	default:
		return "", fmt.Errorf(
			"unknown transport %q (want %q or %q)",
			choice,
			TransportHTTP,
			TransportSSE,
		)
	}
}

// Config describes how to reach a remote MCP server.
type Config struct {
	URL string
	// Transport is TransportHTTP, TransportSSE or TransportAuto.
	Transport string
	// Headers are sent with every request, e.g. a static API key.
	Headers map[string]string
	// OAuth enables OAuth with stored credentials; nil disables it.
	OAuth *transport.OAuthConfig
}

// NewClient creates and starts (but does not initialize) a client.
func NewClient(ctx context.Context, cfg Config) (*client.Client, error) {
	kind, err := ResolveTransport(cfg.URL, cfg.Transport)
	if err != nil {
		return nil, err
	}

	var c *client.Client
	switch kind {
	case TransportSSE:
		opts := []transport.ClientOption{transport.WithHeaders(cfg.Headers)}
		if cfg.OAuth != nil {
			opts = append(opts, transport.WithOAuth(*cfg.OAuth))
		}
		c, err = client.NewSSEMCPClient(cfg.URL, opts...)
	default:
		opts := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(cfg.Headers)}
		if cfg.OAuth != nil {
			opts = append(opts, transport.WithHTTPOAuth(*cfg.OAuth))
		}
		c, err = client.NewStreamableHttpClient(cfg.URL, opts...)
	}
	if err != nil {
		return nil, err
	}
	// The SSE stream lives as long as the context given to Start, which must
	// therefore outlive the request that triggered the connection.
	if err := c.Start(context.WithoutCancel(ctx)); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("connect to %s: %w", cfg.URL, err)
	}
	return c, nil
}

// IsAuthError reports whether err means the server requires (new)
// authorization.
func IsAuthError(err error) bool {
	return client.IsAuthorizationRequiredError(err) ||
		client.IsOAuthAuthorizationRequiredError(err) ||
		errors.Is(err, transport.ErrOAuthAuthorizationRequired)
}

// ParseHeader parses a curl-style "Name: value" header.
func ParseHeader(h string) (string, string, error) {
	name, value, ok := strings.Cut(h, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.ContainsAny(name, " \t") {
		return "", "", fmt.Errorf("header %q must look like \"Name: value\"", h)
	}
	return name, strings.TrimSpace(value), nil
}
