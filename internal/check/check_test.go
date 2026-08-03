package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
)

// The overlap carry is the subtle part: a needle split across a chunk
// boundary must still be found, for every split position and even when the
// needle is longer than the read buffer.
func TestSearchReader(t *testing.T) {
	needles := []string{"NEEDLE", "api/oauth"}
	cases := []struct {
		name    string
		content string
		bufSize int
		want    map[string]bool
	}{
		{"inside one chunk", "xxNEEDLExx api/oauth xx", 64,
			map[string]bool{"NEEDLE": true, "api/oauth": true}},
		{"split across boundary", "xxxxxxNEEDLExxxx", 8, // boundary at E|DLE
			map[string]bool{"NEEDLE": true}},
		{"needle longer than buffer", "xxNEEDLExx", 3,
			map[string]bool{"NEEDLE": true}},
		{"missing", "nothing here", 8, map[string]bool{}},
		{"at the very end", "xxxxxxxxxxNEEDLE", 8, map[string]bool{"NEEDLE": true}},
	}
	for _, c := range cases {
		got := searchReader(strings.NewReader(c.content), needles, c.bufSize)
		for n, want := range c.want {
			if got[n] != want {
				t.Errorf("%s: found[%q] = %v, want %v", c.name, n, got[n], want)
			}
		}
		if len(c.want) == 0 && len(got) != 0 {
			t.Errorf("%s: unexpected finds %v", c.name, got)
		}
	}

	// Exhaustive: split "NEEDLE" at every boundary a 4-byte buffer produces
	// for every prefix length.
	for pad := 0; pad < 12; pad++ {
		content := strings.Repeat("x", pad) + "NEEDLE" + strings.Repeat("y", 3)
		got := searchReader(strings.NewReader(content), []string{"NEEDLE"}, 4)
		if !got["NEEDLE"] {
			t.Errorf("pad %d: needle lost at chunk boundary", pad)
		}
	}

	// A reader that returns data and EOF in the same call (io.Reader allows
	// it) must not drop the final chunk.
	r := iotest.DataErrReader(strings.NewReader("xxxxxxNEEDLE"))
	if got := searchReader(r, []string{"NEEDLE"}, 8); !got["NEEDLE"] {
		t.Error("data+EOF read dropped the final chunk")
	}
}

// checkRouting is deliberately independent of state.json — it must report
// corrupt `.current` and an inherited CLAUDE_CONFIG_DIR even when the state
// document was written by a newer headroom and the state audit
// short-circuits. Run-level coverage of that placement isn't feasible (Run
// spawns claude and talks HTTP), so the function is pinned directly and the
// call site sits outside checkOwnState by review.
func TestCheckRouting(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)

	run := func(environ []string) (ownFails, envLines []string) {
		chk := func(ok bool, label, hint string) {
			if ok {
				envLines = append(envLines, label)
			}
		}
		own := func(ok bool, label, hint string) {
			if !ok {
				ownFails = append(ownFails, label)
			}
		}
		checkRouting(cfg, accts, environ, chk, own)
		return
	}

	// Absent .current: the documented default — no failure, and a clean
	// environment earns no env line.
	if fails, lines := run([]string{"HOME=" + home}); len(fails) != 0 || len(lines) != 0 {
		t.Errorf("clean state: ownFails=%v envLines=%v", fails, lines)
	}

	// Corrupt (empty) .current: an own-state FAIL — headroom's file, never
	// vendor drift.
	if err := os.WriteFile(cfg.CurrentFile(), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if fails, _ := run([]string{"HOME=" + home}); len(fails) != 1 {
		t.Errorf("empty .current: want one own FAIL, got %v", fails)
	}
	if err := os.Remove(cfg.CurrentFile()); err != nil {
		t.Fatal(err)
	}

	// An inherited variable — present-but-empty included, which is
	// unverified vendor territory rather than the verified absent state —
	// earns the ok-with-detail line.
	if _, lines := run([]string{"CLAUDE_CONFIG_DIR=/leak"}); len(lines) != 1 {
		t.Errorf("inherited value: want env line, got %v", lines)
	}
	if _, lines := run([]string{"CLAUDE_CONFIG_DIR="}); len(lines) != 1 {
		t.Errorf("present-but-empty: want env line, got %v", lines)
	}
}

// A non-empty relative inherited value is the stale-wrapper incident's
// signature — unmanaged claude in that shell runs as the primary while
// writing state beside the cwd — and fails as own-state, never as drift.
func TestCheckRoutingFailsRelativeAmbientDir(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "qiushi"}
	accts := accounts.Discover(cfg)

	var ownFails []string
	own := func(ok bool, label, hint string) {
		if !ok {
			ownFails = append(ownFails, label)
		}
	}
	chk := func(bool, string, string) {}
	checkRouting(cfg, accts, []string{"CLAUDE_CONFIG_DIR=yan@planlab.ai"}, chk, own)
	found := false
	for _, l := range ownFails {
		if strings.Contains(l, "relative") {
			found = true
		}
	}
	if !found {
		t.Errorf("relative ambient dir did not fail: %v", ownFails)
	}
}

// The topology assertion runs per extra account through the same verifier
// launch refuses with, so the gate and the report cannot disagree.
func TestCheckRoutingAssertsTopology(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "qiushi"}
	extra := filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")
	if err := os.MkdirAll(filepath.Join(extra, "projects"), 0o755); err != nil { // real dir: the fork
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.ProjectsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)

	var ownFails []string
	own := func(ok bool, label, hint string) {
		if !ok {
			ownFails = append(ownFails, label)
		}
	}
	checkRouting(cfg, accts, nil, func(bool, string, string) {}, own)
	found := false
	for _, l := range ownFails {
		if strings.Contains(l, "topology[yan@planlab.ai]") {
			found = true
		}
	}
	if !found {
		t.Errorf("forked topology not reported: %v", ownFails)
	}
}
