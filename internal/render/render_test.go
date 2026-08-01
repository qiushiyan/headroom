package render

import (
	"regexp"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/usage"
)

func TestBar(t *testing.T) {
	cases := []struct {
		pct    int
		filled int
	}{
		{0, 0}, {3, 1}, {50, 10}, {56, 11}, {100, 20},
		{130, 20}, // clamped high
		{-5, 0},   // clamped low
	}
	for _, c := range cases {
		bar := Bar(c.pct)
		if got := strings.Count(bar, "█"); got != c.filled {
			t.Errorf("Bar(%d): %d filled, want %d", c.pct, got, c.filled)
		}
		if got := strings.Count(bar, "░"); got != BarWidth-c.filled {
			t.Errorf("Bar(%d): %d empty, want %d", c.pct, got, BarWidth-c.filled)
		}
	}
}

func TestResetPhrase(t *testing.T) {
	now := int64(1_754_000_000)
	if got := ResetPhrase(0, now); got != "resets ?" {
		t.Errorf("unknown: %q", got)
	}
	if got := ResetPhrase(now-10, now); got != "resetting…" {
		t.Errorf("past: %q", got)
	}
	sameDay := regexp.MustCompile(`^resets \d{2}:\d{2} \(in (.+)\)$`)
	withDay := regexp.MustCompile(`^resets [A-Z][a-z]{2} \d{2}:\d{2} \(in (.+)\)$`)
	cases := []struct {
		rem int64
		re  *regexp.Regexp
		in  string
	}{
		{90, sameDay, "1m"},
		{3*3600 + 120, sameDay, "3h 2m"},
		{2*86400 + 3*3600, withDay, "2d 3h"},
	}
	for _, c := range cases {
		got := ResetPhrase(now+c.rem, now)
		m := c.re.FindStringSubmatch(got)
		if m == nil {
			t.Errorf("ResetPhrase(+%d) = %q, want match %v", c.rem, got, c.re)
			continue
		}
		if m[1] != c.in {
			t.Errorf("ResetPhrase(+%d) = %q, want remaining %q", c.rem, got, c.in)
		}
	}
}

func TestLimitRowPlain(t *testing.T) {
	p := NewPalette(false)
	row := usage.Row{Label: "5h session", Percent: 56, Severity: "normal"}
	got := p.LimitRow(row, 0)
	if !strings.HasPrefix(got, "  5h session       [") {
		t.Errorf("label padding off: %q", got)
	}
	if !strings.Contains(got, "]  56%  resets ?") {
		t.Errorf("row = %q", got)
	}
}

func TestHeaderLine(t *testing.T) {
	p := NewPalette(false)
	v := AccountView{Label: "a@b.c", Plan: "max 20x", Launcher: "x-a", Current: true}
	if got := p.HeaderLine(v); got != "a@b.c (max 20x · x-a)  ← x" {
		t.Errorf("header = %q", got)
	}
	v = AccountView{Label: "a@b.c", Launcher: "x-a"}
	if got := p.HeaderLine(v); got != "a@b.c  x-a" {
		t.Errorf("planless header = %q", got)
	}
	v = AccountView{Label: "real@b.c", DirMismatch: "dir@b.c", Launcher: "x-a"}
	if got := p.HeaderLine(v); !strings.Contains(got, "real@b.c (dir says dir@b.c!)") {
		t.Errorf("mismatch header = %q", got)
	}
}
