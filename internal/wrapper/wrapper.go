// Package wrapper runs the shell commands configured as wrapper tools.
package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fpt/go-mcpproxy/internal/config"
)

// Defaults for unset wrapper settings.
const (
	DefaultTimeout   = 10 * time.Minute
	DefaultMaxOutput = 100_000
)

// killGrace is how long a command may take to exit after being killed
// before its output pipes are closed regardless.
const killGrace = 5 * time.Second

// Result is the outcome of running a wrapper command.
type Result struct {
	// ExitCode is the command's exit status; -1 if it did not exit normally
	// (killed by a signal or the timeout, or could not be started).
	ExitCode int `json:"exit_code"`
	// Stdout and Stderr are the command's output streams, kept separately.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// StdoutTruncated and StderrTruncated report that only the last
	// max_output bytes were kept.
	StdoutTruncated bool `json:"stdout_truncated,omitempty"`
	StderrTruncated bool `json:"stderr_truncated,omitempty"`
	// Error explains a run that did not produce an exit code, e.g. a timeout.
	Error string `json:"error,omitempty"`
}

// Run executes the wrapper's command and waits for it. Stdin is empty. It
// never returns an error: failures to start or finish are reported in
// Result.Error with ExitCode -1.
func Run(ctx context.Context, w config.Wrapper) Result {
	timeout := w.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxOutput := w.MaxOutput
	if maxOutput == 0 {
		maxOutput = DefaultMaxOutput
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := shellCommand(ctx, w.Command)
	dir, err := workingDir(w.Dir)
	if err != nil {
		return Result{ExitCode: -1, Error: err.Error()}
	}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for _, k := range slices.Sorted(maps.Keys(w.Env)) {
		cmd.Env = append(cmd.Env, k+"="+config.ExpandEnv(w.Env[k]))
	}
	stdout, stderr := newTail(maxOutput), newTail(maxOutput)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = killGrace
	killProcessGroup(cmd)

	err = cmd.Run()
	res := Result{
		ExitCode:        cmd.ProcessState.ExitCode(),
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}
	if cmd.ProcessState == nil {
		res.ExitCode = -1
	}
	var exitErr *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Error = fmt.Sprintf("timed out after %s; the command was killed", timeout)
	case ctx.Err() != nil:
		res.Error = "cancelled; the command was killed"
	case err != nil && !errors.As(err, &exitErr):
		res.Error = err.Error()
	}
	return res
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

func workingDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	abs, err := config.AbsPath(config.ExpandEnv(dir))
	if err != nil {
		return "", fmt.Errorf("working directory %q: %w", dir, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("working directory %q does not exist", abs)
	}
	return abs, nil
}

// tail is an io.Writer that keeps the last limit bytes.
type tail struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func newTail(limit int) *tail { return &tail{limit: limit} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "�")
}

// Tool returns the MCP tool definition for a wrapper.
func Tool(name string, w config.Wrapper) mcp.Tool {
	desc := w.Description
	if desc == "" {
		where := "the project directory"
		if w.Dir != "" {
			where = w.Dir
		}
		desc = fmt.Sprintf("Run `%s` in %s.", w.Command, where)
	}
	desc = strings.TrimSpace(desc)
	if !strings.HasSuffix(desc, ".") && !strings.HasSuffix(desc, "!") &&
		!strings.HasSuffix(desc, "?") {
		desc += "."
	}
	desc += " Takes no arguments. Returns the exit code, stdout and stderr separately; " +
		"a non-zero exit code is reported as an error."
	return mcp.NewTool(name,
		mcp.WithDescription(desc),
		mcp.WithOutputSchema[Result](),
		mcp.WithTitleAnnotation(w.Command),
		mcp.WithOpenWorldHintAnnotation(false),
	)
}

// Handler returns the tool handler running a wrapper.
func Handler(w config.Wrapper) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return ToolResult(Run(ctx, w)), nil
	}
}

// ToolResult converts a Result into a tool result with structured content
// and the same JSON as text; it is an error unless the command exited 0.
func ToolResult(res Result) *mcp.CallToolResult {
	text, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	out := mcp.NewToolResultStructured(res, string(text))
	out.IsError = res.ExitCode != 0 || res.Error != ""
	return out
}
