package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
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

// TestMain runs the package in an environment headroom's own variables do not
// reach: a test that wants one sets it with t.Setenv. The shell this suite
// runs from is often a managed session — CLAUDE_CONFIG_DIR exported, ssh, a
// HEADROOM_* override — and each of those changes what a launch prints and
// where it routes, so a test that passed only because another test had
// cleared the variable first was order-dependent.
func TestMain(m *testing.M) {
	ambient := []string{"BROWSER", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"}
	for _, v := range []config.Vendor{config.Claude, config.Codex} {
		ambient = append(ambient, v.EnvPolicy().Strip...)
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "HEADROOM_") {
			ambient = append(ambient, name)
		}
	}
	for _, v := range ambient {
		os.Unsetenv(v)
	}
	// A launch under auto leaves a detached refresh behind; in a test binary
	// that would re-execute the suite. The tests that observe the edge set
	// their own.
	startRefresh = func(config.Scope) error { return nil }
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
