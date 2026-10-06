// Package apptest provides test helpers shared across packages.
package apptest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// BuildEchoServer compiles the echoserver fixture with the given variant to
// out, replacing it atomically the way `go build -o` does.
func BuildEchoServer(t *testing.T, out, variant string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(file), "..", "app", "testdata", "echoserver")

	tmp := out + ".tmp"
	cmd := exec.Command("go", "build", "-o", tmp, "-ldflags", "-X main.variant="+variant, ".")
	cmd.Dir = src
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build echoserver %s: %v\n%s", variant, err, b)
	}
	if err := os.Rename(tmp, out); err != nil {
		t.Fatal(err)
	}
}
