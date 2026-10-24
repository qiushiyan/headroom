package app

import (
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tui"
)

// The rename field opens on the transcript's title, which another program
// wrote, and search echoes what was typed: drawn, each is one row with no
// control bytes in it — an ESC there clears the screen, a newline shears the
// frame — while the edit buffer keeps the text as read, for the commit to
// compare and sanitize itself.
func TestTheInputLineDrawsVendorTextSafely(t *testing.T) {
	const title = "fix\x1b[2J the\nbug\tnow"
	cases := []struct {
		name string
		ui   func() *resumeUI
	}{
		{"rename opened on the title", func() *resumeUI {
			ui := &resumeUI{mode: modeRename}
			ui.edit.SetString(title)
			return ui
		}},
		{"rename with the cursor on the escape", func() *resumeUI {
			ui := &resumeUI{mode: modeRename}
			ui.edit.SetString(title)
			ui.edit.Handle(tui.Key{Kind: tui.KeyHome})
			for range len("fix") {
				ui.edit.Handle(tui.Key{Kind: tui.KeyRight})
			}
			return ui
		}},
		{"search", func() *resumeUI {
			ui := &resumeUI{mode: modeSearch}
			ui.edit.SetString(title)
			return ui
		}},
		{"the kept filter", func() *resumeUI { return &resumeUI{query: title} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui := c.ui()
			line := ui.inputLine() // the zero palette adds no escapes of its own
			if strings.ContainsAny(line, "\x1b\n\r\t") {
				t.Errorf("drawn line carries control bytes: %q", line)
			}
			for _, word := range []string{"fix", "the", "bug", "now"} {
				if !strings.Contains(line, word) {
					t.Errorf("drawn line %q lost %q", line, word)
				}
			}
			if ui.mode != modeNormal && ui.edit.String() != title {
				t.Errorf("edit buffer = %q, want the title as read", ui.edit.String())
			}
		})
	}

	// A cursor on a rune that draws as nothing still shows where it is.
	ui := &resumeUI{mode: modeRename, p: render.NewPalette(true)}
	ui.edit.SetString("a\x1bb")
	ui.edit.Handle(tui.Key{Kind: tui.KeyLeft})
	ui.edit.Handle(tui.Key{Kind: tui.KeyLeft})
	if line := ui.inputLine(); !strings.Contains(line, ui.p.Rev+" "+ui.p.Rst) {
		t.Errorf("cursor on a dropped rune drew no cell: %q", line)
	}

	// And the empty result names the query the same way.
	ui = &resumeUI{query: title}
	lines, _, _ := ui.rowLines(80, 0)
	if got := strings.Join(lines, "\n"); strings.ContainsAny(got, "\x1b\r\t") || strings.Count(got, "\n") != len(lines)-1 {
		t.Errorf("no-match line carries control bytes: %q", got)
	}
}

// Search has to find a session by either branch: the one it ran on, which is
// what you remember, and the one its checkout is on now, which is what you
// see in the row.
func TestMatchesFindsEitherBranch(t *testing.T) {
	s := &sessions.Session{
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
		Tail: sessions.Tail{Branch: "skill/baton-pointer-and-onboarding-order"},
	}
	for _, q := range []string{"develop", "baton-pointer"} {
		if !matches(s, q) {
			t.Errorf("query %q missed the session", q)
		}
	}
}
