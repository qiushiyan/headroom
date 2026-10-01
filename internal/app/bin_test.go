package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// built is the headroom binary, built once for every test that runs it: the
// properties those tests check — a process that outlives the launch, a launch
// with no terminal under another HOME, a refresh killed mid-request — belong
// to processes, not to functions.
var built struct {
	once      sync.Once
	dir, path string
	out       []byte
	err       error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if built.dir != "" {
		os.RemoveAll(built.dir)
	}
	os.Exit(code)
}

func headroomBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary")
	}
	built.once.Do(func() {
		if built.dir, built.err = os.MkdirTemp("", "headroom-bin"); built.err != nil {
			return
		}
		built.path = filepath.Join(built.dir, "headroom")
		built.out, built.err = exec.Command("go", "build", "-o", built.path, "../../cmd/headroom").CombinedOutput()
	})
	if built.err != nil {
		t.Fatalf("build: %v\n%s", built.err, built.out)
	}
	return built.path
}
