package accounts

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

// No share mode links a login between two dirs: not the whitelist, and not a
// config package that happens to hold a login, a history or a registry.
func TestNoLoginIsLinkedBetweenDirs(t *testing.T) {
	for name, opt := range map[string]func(cfg config.Scope) SeedOptions{
		"whitelist": func(cfg config.Scope) SeedOptions {
			return SeedOptions{ShareFrom: cfg.PrimaryDir(), ShareNames: SharedConfigEntries(config.Claude)}
		},
		"every entry": func(cfg config.Scope) SeedOptions { return SeedOptions{ShareFrom: cfg.PrimaryDir()} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.ForHome(t.TempDir()).Claude
			src := cfg.PrimaryDir()
			for _, d := range []string{"skills", "sessions", "projects"} {
				if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range []string{"settings.json", ".credentials.json", ".claude.json", "history.jsonl"} {
				if err := os.WriteFile(filepath.Join(src, f), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			seeded, err := Seed(cfg, "a@x.com", opt(cfg))
			dir, shared, kept := seeded.Dir, seeded.Shared, seeded.Kept
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"settings.json", "skills"} {
				if !slices.Contains(shared, n) {
					t.Errorf("%s not shared: %v", n, shared)
				}
			}
			if target, _ := os.Readlink(filepath.Join(dir, "settings.json")); target != filepath.Join(src, "settings.json") {
				t.Errorf("settings.json → %q, want the source's", target)
			}
			if name == "whitelist" {
				for _, n := range shared {
					if !slices.Contains(SharedConfigEntries(config.Claude), n) {
						t.Errorf("%s shared outside the whitelist", n)
					}
				}
			}
			for _, n := range []string{".credentials.json", ".claude.json", "history.jsonl", "sessions"} {
				if slices.Contains(shared, n) {
					t.Errorf("%s was linked", n)
				}
				if fi, err := os.Lstat(filepath.Join(dir, n)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					t.Errorf("%s/%s is a link", dir, n)
				}
			}
			if name == "every entry" && !slices.Contains(kept, ".credentials.json") {
				t.Errorf("kept = %v — a login the source held must be named as kept", kept)
			}
			if err := VerifyTopology(Account{Scope: cfg, ConfigDir: dir, Name: "a@x.com"}); err != nil {
				t.Errorf("topology: %v", err)
			}
		})
	}
}
