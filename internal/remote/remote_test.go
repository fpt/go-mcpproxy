package remote_test

import (
	"testing"

	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTransport(t *testing.T) {
	tests := []struct {
		url, choice, want string
		wantErr           bool
	}{
		{"https://x/mcp", "", remote.TransportHTTP, false},
		{"https://x/sse", "", remote.TransportSSE, false},
		{"https://x/v1/sse/", "", remote.TransportSSE, false},
		{"https://x/ssex", "", remote.TransportHTTP, false},
		{"https://x/sse", "http", remote.TransportHTTP, false},
		{"https://x/mcp", "sse", remote.TransportSSE, false},
		{"https://x/mcp", "ws", "", true},
		{"ftp://x/mcp", "", "", true},
		{"https:///mcp", "", "", true},
	}
	for _, tt := range tests {
		got, err := remote.ResolveTransport(tt.url, tt.choice)
		if tt.wantErr {
			assert.Error(t, err, tt.url)
			continue
		}
		require.NoError(t, err, tt.url)
		assert.Equal(t, tt.want, got, tt.url)
	}
}

func TestParseHeader(t *testing.T) {
	name, value, err := remote.ParseHeader("Authorization: Bearer a:b")
	require.NoError(t, err)
	assert.Equal(t, "Authorization", name)
	assert.Equal(t, "Bearer a:b", value)

	for _, bad := range []string{"NoColon", ": value", "Bad Name: v"} {
		_, _, err := remote.ParseHeader(bad)
		assert.Error(t, err, bad)
	}
}
