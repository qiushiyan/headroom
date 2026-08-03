package launch

import (
	"slices"
	"testing"
)

func extra(t *testing.T, dir string) Target {
	t.Helper()
	tgt, err := Extra(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func TestEnv(t *testing.T) {
	cases := []struct {
		name   string
		base   []string
		target Target
		want   []string
	}{
		{
			name:   "primary strips inherited var",
			base:   []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/leak", "TERM=xterm"},
			target: Primary(),
			want:   []string{"HOME=/u", "TERM=xterm"},
		},
		{
			name:   "primary with clean base changes nothing",
			base:   []string{"HOME=/u", "TERM=xterm"},
			target: Primary(),
			want:   []string{"HOME=/u", "TERM=xterm"},
		},
		{
			name:   "extra account gets exactly one entry",
			base:   []string{"HOME=/u"},
			target: extra(t, "/accts/a@x.com"),
			want:   []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/a@x.com"},
		},
		{
			name:   "extra account replaces a different inherited value",
			base:   []string{"CLAUDE_CONFIG_DIR=/leak", "HOME=/u"},
			target: extra(t, "/accts/a@x.com"),
			want:   []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/a@x.com"},
		},
		{
			name:   "duplicate inherited entries are all removed",
			base:   []string{"CLAUDE_CONFIG_DIR=/one", "HOME=/u", "CLAUDE_CONFIG_DIR=/two"},
			target: Primary(),
			want:   []string{"HOME=/u"},
		},
		{
			name:   "prefix-named variables survive",
			base:   []string{"CLAUDE_CONFIG_DIR_BACKUP=/keep", "HOME=/u"},
			target: Primary(),
			want:   []string{"CLAUDE_CONFIG_DIR_BACKUP=/keep", "HOME=/u"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.target.Env(c.base); !slices.Equal(got, c.want) {
				t.Errorf("Env(%v) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}

func TestEnvDoesNotMutateBase(t *testing.T) {
	base := []string{"CLAUDE_CONFIG_DIR=/leak", "HOME=/u"}
	orig := slices.Clone(base)
	extra(t, "/accts/a@x.com").Env(base)
	if !slices.Equal(base, orig) {
		t.Errorf("Env mutated its input: %v", base)
	}
}

func TestExtraRefusesEmptyDir(t *testing.T) {
	if _, err := Extra(""); err == nil {
		t.Error("Extra(\"\") must refuse: it would build a primary-shaped environment under an extra-shaped call")
	}
}

func TestAmbient(t *testing.T) {
	if v, present := Ambient([]string{"HOME=/u"}); present || v != "" {
		t.Errorf("Ambient(clean) = (%q, %v), want absent", v, present)
	}
	if v, present := Ambient([]string{"CLAUDE_CONFIG_DIR=/leak"}); !present || v != "/leak" {
		t.Errorf("Ambient = (%q, %v), want (/leak, true)", v, present)
	}
	// Present-but-empty is present: it is unverified vendor territory, not
	// the verified absent state, and hiding the difference is how a
	// diagnostic goes silent over it.
	if v, present := Ambient([]string{"CLAUDE_CONFIG_DIR="}); !present || v != "" {
		t.Errorf("Ambient(present-empty) = (%q, %v), want (\"\", true)", v, present)
	}
}

func TestConflicts(t *testing.T) {
	cases := []struct {
		name   string
		base   []string
		target Target
		want   bool
	}{
		{"absent never conflicts", []string{"HOME=/u"}, Primary(), false},
		// The primary is selected only by absence, so any present value —
		// its own real path and the empty string included — conflicts.
		{"primary vs other dir", []string{"CLAUDE_CONFIG_DIR=/leak"}, Primary(), true},
		{"primary vs its own real path", []string{"CLAUDE_CONFIG_DIR=/u/.claude"}, Primary(), true},
		{"primary vs present-empty", []string{"CLAUDE_CONFIG_DIR="}, Primary(), true},
		{"extra vs exactly its dir", []string{"CLAUDE_CONFIG_DIR=/accts/a"}, extra(t, "/accts/a"), false},
		{"extra vs a different dir", []string{"CLAUDE_CONFIG_DIR=/accts/b"}, extra(t, "/accts/a"), true},
		{"extra vs present-empty", []string{"CLAUDE_CONFIG_DIR="}, extra(t, "/accts/a"), true},
		{"extra vs absent", []string{"HOME=/u"}, extra(t, "/accts/a"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := c.target.Conflicts(c.base); got != c.want {
				t.Errorf("Conflicts(%v) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}

// A relative dir is the reproduced chimera: claude reads the *default*
// Keychain item (the primary's login) while writing state beside the cwd.
// Both constructors refuse it, so Env can never emit one.
func TestExtraAndForRefuseRelativeDirs(t *testing.T) {
	for _, dir := range []string{"yan@planlab.ai", "./x", "../x"} {
		if _, err := Extra(dir); err == nil {
			t.Errorf("Extra(%q) accepted a relative dir", dir)
		}
		if _, err := For(dir); err == nil {
			t.Errorf("For(%q) accepted a relative dir", dir)
		}
	}
	if tgt, err := For(""); err != nil || !tgt.IsPrimary() {
		t.Errorf("For(\"\") = (%v, %v), want the primary", tgt, err)
	}
	if tgt, err := For("/abs/dir"); err != nil || tgt.IsPrimary() {
		t.Errorf("For(\"/abs/dir\") = (%v, %v), want an extra", tgt, err)
	}
}
