package app_test

import (
	"testing"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
)

func searchFixture() []mcp.Tool {
	return []mcp.Tool{
		mcp.NewTool("read_file", mcp.WithDescription("Read a file from disk"),
			mcp.WithString("path")),
		mcp.NewTool("write_file", mcp.WithDescription("Write a file to disk"),
			mcp.WithString("path"), mcp.WithString("content")),
		mcp.NewTool("listDirectory", mcp.WithDescription("List entries of a directory")),
		mcp.NewTool("getHTTPStatus", mcp.WithDescription("Fetch a URL and report the status")),
		mcp.NewTool("search", mcp.WithDescription("Full text search")),
	}
}

func names(tools []mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func TestSearchTools(t *testing.T) {
	tools := searchFixture()
	tests := []struct {
		name        string
		query       string
		max         int
		want        []string
		wantTotal   int
		wantMissing []string
	}{
		{
			"select keeps order", "select:write_file, read_file", 5,
			[]string{"write_file", "read_file"},
			2, nil,
		},
		{
			"select reports missing", "select:read_file,nope", 5,
			[]string{"read_file"},
			1,
			[]string{"nope"},
		},
		{
			"select ignores limit", "select:read_file,write_file", 1,
			[]string{"read_file", "write_file"},
			2, nil,
		},
		{"exact name ranks first", "search", 5, []string{"search"}, 1, nil},
		{"name word beats description", "file", 5, []string{"read_file", "write_file"}, 2, nil},
		{"description match", "disk", 5, []string{"read_file", "write_file"}, 2, nil},
		{"parameter name match", "content", 5, []string{"write_file"}, 1, nil},
		{"camelCase words", "directory", 5, []string{"listDirectory"}, 1, nil},
		{"acronym split", "http", 5, []string{"getHTTPStatus"}, 1, nil},
		{"required term filters", "+write file", 5, []string{"write_file"}, 1, nil},
		{"required only", "+file", 5, []string{"read_file", "write_file"}, 2, nil},
		{"limit applies, total does not", "file", 1, []string{"read_file"}, 2, nil},
		{"case insensitive", "READ", 5, []string{"read_file"}, 1, nil},
		{"no match", "kubernetes", 5, nil, 0, nil},
		{
			"empty lists all", "", 0,
			[]string{"getHTTPStatus", "listDirectory", "read_file", "search", "write_file"},
			5, nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := app.SearchTools(tools, tt.query, tt.max)
			assert.Equal(t, tt.want, nilIfEmpty(names(res.Tools)))
			assert.Equal(t, tt.wantTotal, res.Total)
			assert.Equal(t, tt.wantMissing, res.Missing)
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
