package wrapper_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/wrapper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSeparatesStreamsAndExitCode(t *testing.T) {
	res := wrapper.Run(context.Background(), config.Wrapper{
		Command: `echo out; echo err >&2; exit 3`,
	})
	assert.Equal(t, 3, res.ExitCode)
	assert.Equal(t, "out\n", res.Stdout)
	assert.Equal(t, "err\n", res.Stderr)
	assert.Empty(t, res.Error)

	tr := wrapper.ToolResult(res)
	assert.True(t, tr.IsError)
	assert.Equal(t, res, tr.StructuredContent)
}

func TestRunSuccess(t *testing.T) {
	res := wrapper.Run(context.Background(), config.Wrapper{Command: "true"})
	assert.Equal(t, 0, res.ExitCode)
	assert.False(t, wrapper.ToolResult(res).IsError)
}

func TestRunDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	t.Setenv("MCPPROXY_TEST_GREETING", "hello")
	res := wrapper.Run(context.Background(), config.Wrapper{
		Command: `pwd -P; echo "$GREETING"; cat`,
		Dir:     dir,
		Env:     map[string]string{"GREETING": "${MCPPROXY_TEST_GREETING} world"},
	})
	require.Equal(t, 0, res.ExitCode, res.Stderr)
	// stdin is empty, so cat returns immediately.
	assert.Equal(t, real+"\nhello world\n", res.Stdout)
}

func TestRunMissingDir(t *testing.T) {
	res := wrapper.Run(context.Background(), config.Wrapper{
		Command: "true",
		Dir:     filepath.Join(t.TempDir(), "missing"),
	})
	assert.Equal(t, -1, res.ExitCode)
	assert.Contains(t, res.Error, "does not exist")
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	start := time.Now()
	res := wrapper.Run(context.Background(), config.Wrapper{
		// The child would create the marker if it outlived the shell.
		Command: `echo started; (sleep 1; touch ` + marker + `) & sleep 30`,
		Timeout: 200 * time.Millisecond,
	})
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, -1, res.ExitCode)
	assert.Contains(t, res.Error, "timed out after 200ms")
	assert.Equal(t, "started\n", res.Stdout, "output before the timeout is kept")

	time.Sleep(1500 * time.Millisecond)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "child process outlived the timeout")
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res := wrapper.Run(ctx, config.Wrapper{Command: "sleep 30"})
	assert.Equal(t, -1, res.ExitCode)
	assert.NotEmpty(t, res.Error)
}

func TestRunTruncatesKeepingTail(t *testing.T) {
	res := wrapper.Run(context.Background(), config.Wrapper{
		Command:   `for i in $(seq 1 1000); do echo line$i; done`,
		MaxOutput: 20,
	})
	require.Equal(t, 0, res.ExitCode)
	assert.True(t, res.StdoutTruncated)
	assert.Len(t, res.Stdout, 20)
	assert.True(t, strings.HasSuffix(res.Stdout, "line1000\n"))
}

func TestToolDefinition(t *testing.T) {
	tool := wrapper.Tool("make", config.Wrapper{Command: "make test"})
	assert.Equal(t, "make", tool.Name)
	assert.Contains(t, tool.Description, "Run `make test` in the project directory.")
	assert.Empty(t, tool.InputSchema.Properties)

	custom := wrapper.Tool(
		"lint",
		config.Wrapper{Command: "make lint", Description: "Lint the code."},
	)
	assert.True(t, strings.HasPrefix(custom.Description, "Lint the code. Takes no arguments."))

	noPeriod := wrapper.Tool("x", config.Wrapper{Command: "true", Description: "Do x"})
	assert.True(t, strings.HasPrefix(noPeriod.Description, "Do x. Takes no arguments."))
}
