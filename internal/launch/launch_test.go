package launch

import (
	"slices"
	"testing"
)

func TestEnv(t *testing.T) {
	cases := []struct {
		name      string
		base      []string
		configDir string
		want      []string
	}{
		{
			name:      "primary strips inherited var",
			base:      []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/leak", "TERM=xterm"},
			configDir: "",
			want:      []string{"HOME=/u", "TERM=xterm"},
		},
		{
			name:      "primary with clean base changes nothing",
			base:      []string{"HOME=/u", "TERM=xterm"},
			configDir: "",
			want:      []string{"HOME=/u", "TERM=xterm"},
		},
		{
			name:      "extra account gets exactly one entry",
			base:      []string{"HOME=/u"},
			configDir: "/accts/a@x.com",
			want:      []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/a@x.com"},
		},
		{
			name:      "extra account replaces a different inherited value",
			base:      []string{"CLAUDE_CONFIG_DIR=/leak", "HOME=/u"},
			configDir: "/accts/a@x.com",
			want:      []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/a@x.com"},
		},
		{
			name: "duplicate inherited entries are all removed",
			base: []string{"CLAUDE_CONFIG_DIR=/one", "HOME=/u", "CLAUDE_CONFIG_DIR=/two"},
			want: []string{"HOME=/u"},
		},
		{
			name: "prefix-named variables survive",
			base: []string{"CLAUDE_CONFIG_DIR_BACKUP=/keep", "HOME=/u"},
			want: []string{"CLAUDE_CONFIG_DIR_BACKUP=/keep", "HOME=/u"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Env(c.base, c.configDir); !slices.Equal(got, c.want) {
				t.Errorf("Env(%v, %q) = %v, want %v", c.base, c.configDir, got, c.want)
			}
		})
	}
}

func TestEnvDoesNotMutateBase(t *testing.T) {
	base := []string{"CLAUDE_CONFIG_DIR=/leak", "HOME=/u"}
	orig := slices.Clone(base)
	Env(base, "/accts/a@x.com")
	if !slices.Equal(base, orig) {
		t.Errorf("Env mutated its input: %v", base)
	}
}

func TestInherited(t *testing.T) {
	if got := Inherited([]string{"HOME=/u"}); got != "" {
		t.Errorf("Inherited(clean) = %q, want empty", got)
	}
	if got := Inherited([]string{"HOME=/u", "CLAUDE_CONFIG_DIR=/leak"}); got != "/leak" {
		t.Errorf("Inherited = %q, want /leak", got)
	}
}
