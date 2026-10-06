//go:build !unix

package wrapper

import "os/exec"

// killProcessGroup is a no-op where process groups are unavailable;
// cancellation kills only the shell.
func killProcessGroup(*exec.Cmd) {}
