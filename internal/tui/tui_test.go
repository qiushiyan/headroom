package tui

import (
	"slices"
	"testing"
)

func feed(d *Decoder, chunks ...string) []Key {
	var keys []Key
	for _, c := range chunks {
		keys = append(keys, d.Feed([]byte(c))...)
	}
	return keys
}

func TestDecoderWholeChunks(t *testing.T) {
	cases := []struct {
		in   string
		want []Key
	}{
		{"\x1b[A", []Key{{Kind: KeyUp}}},
		{"\x1b[B", []Key{{Kind: KeyDown}}},
		{"\x1b[C", []Key{{Kind: KeyRight}}},
		{"\x1b[D", []Key{{Kind: KeyLeft}}},
		{"\x1bOA", []Key{{Kind: KeyUp}}}, // application cursor mode
		{"\x1b[H", []Key{{Kind: KeyHome}}},
		{"\x1b[3~", []Key{{Kind: KeyDelete}}},
		{"\x1b[5~", []Key{{Kind: KeyPgUp}}},
		{"\x1b[6~", []Key{{Kind: KeyPgDn}}},
		{"k", []Key{{KeyRune, 'k'}}},
		{"j", []Key{{KeyRune, 'j'}}},
		{"/", []Key{{KeyRune, '/'}}},
		{" ", []Key{{KeyRune, ' '}}},
		{"\r", []Key{{Kind: KeyEnter}}},
		{"\n", []Key{{Kind: KeyEnter}}},
		{"\t", []Key{{Kind: KeyTab}}},
		{"\x7f", []Key{{Kind: KeyBackspace}}},
		{"\x03", []Key{{KeyCtrl, 'c'}}},
		{"\x04", []Key{{KeyCtrl, 'd'}}},
		{"\x15", []Key{{KeyCtrl, 'u'}}},
		{"jjk", []Key{{KeyRune, 'j'}, {KeyRune, 'j'}, {KeyRune, 'k'}}},
		{"改", []Key{{KeyRune, '改'}}},
		{"\x1b[1;5C", []Key{{Kind: KeyRight}}}, // params consumed, final decides
	}
	for _, c := range cases {
		var d Decoder
		if got := feed(&d, c.in); !slices.Equal(got, c.want) {
			t.Errorf("Feed(%q) = %v, want %v", c.in, got, c.want)
		}
		if d.Pending() {
			t.Errorf("Feed(%q): decoder left pending", c.in)
		}
	}
}

// A terminal may split an escape sequence across reads at any byte; every
// split of ESC [ A must still decode as one arrow key, not an Escape.
func TestDecoderSplitSequences(t *testing.T) {
	cases := []struct {
		chunks []string
		want   []Key
	}{
		{[]string{"\x1b", "[A"}, []Key{{Kind: KeyUp}}},
		{[]string{"\x1b[", "A"}, []Key{{Kind: KeyUp}}},
		{[]string{"\x1b", "[", "A"}, []Key{{Kind: KeyUp}}},
		{[]string{"\x1b", "[B"}, []Key{{Kind: KeyDown}}},
		{[]string{"\x1b", "OA"}, []Key{{Kind: KeyUp}}},
		{[]string{"\x1b[3", "~"}, []Key{{Kind: KeyDelete}}},
	}
	for _, c := range cases {
		var d Decoder
		got := feed(&d, c.chunks...)
		if !slices.Equal(got, c.want) {
			t.Errorf("feed(%q) = %v, want %v", c.chunks, got, c.want)
		}
		if d.Pending() {
			t.Errorf("feed(%q): decoder left pending", c.chunks)
		}
		if extra := d.Flush(); extra != nil {
			t.Errorf("feed(%q): Flush after complete sequence = %v", c.chunks, extra)
		}
	}
}

// A multi-byte rune split across reads reassembles into one KeyRune — the
// rename and search modes take real text, which arrives however the pty
// chunks it.
func TestDecoderSplitRunes(t *testing.T) {
	var d Decoder
	b := []byte("改") // e6 94 b9
	var got []Key
	got = append(got, d.Feed(b[:1])...)
	got = append(got, d.Feed(b[1:2])...)
	got = append(got, d.Feed(b[2:])...)
	want := []Key{{KeyRune, '改'}}
	if !slices.Equal(got, want) {
		t.Errorf("split rune = %v, want %v", got, want)
	}
	// A dangling fragment is dropped on flush, never surfaced as garbage.
	d = Decoder{}
	d.Feed(b[:2])
	if got := d.Flush(); got != nil {
		t.Errorf("dangling fragment Flush = %v, want nil", got)
	}
}

// A bare ESC is only the Escape key once input pauses (Flush), never eagerly.
func TestDecoderLoneEsc(t *testing.T) {
	var d Decoder
	if got := d.Feed([]byte{0x1b}); got != nil {
		t.Errorf("lone ESC decoded eagerly: %v", got)
	}
	if !d.Pending() {
		t.Fatal("ESC should be held pending")
	}
	if got := d.Flush(); !slices.Equal(got, []Key{{Kind: KeyEsc}}) {
		t.Errorf("Flush = %v, want Esc", got)
	}
	if d.Pending() {
		t.Error("Flush should clear pending")
	}

	// A dangling sequence start also resolves to Escape on pause.
	d = Decoder{}
	feed(&d, "\x1b[")
	if got := d.Flush(); !slices.Equal(got, []Key{{Kind: KeyEsc}}) {
		t.Errorf("dangling ESC[: Flush = %v, want Esc", got)
	}
}

func TestDecoderEscThenOtherKeys(t *testing.T) {
	// ESC followed by a non-sequence byte: the ESC was a real keypress.
	var d Decoder
	if got := feed(&d, "\x1bj"); !slices.Equal(got, []Key{{Kind: KeyEsc}, {KeyRune, 'j'}}) {
		t.Errorf("ESC j = %v", got)
	}
	// ESC ESC: first resolves as Escape, second held for Flush.
	d = Decoder{}
	if got := feed(&d, "\x1b\x1b"); !slices.Equal(got, []Key{{Kind: KeyEsc}}) {
		t.Errorf("ESC ESC = %v", got)
	}
	if got := d.Flush(); !slices.Equal(got, []Key{{Kind: KeyEsc}}) {
		t.Errorf("ESC ESC Flush = %v", got)
	}
}

func TestLineEdit(t *testing.T) {
	var e LineEdit
	for _, r := range "标题x" {
		e.Handle(Key{KeyRune, r})
	}
	if e.String() != "标题x" {
		t.Fatalf("typed = %q", e.String())
	}
	// Backspace removes one whole rune, however many bytes it took.
	e.Handle(Key{Kind: KeyBackspace})
	e.Handle(Key{Kind: KeyBackspace})
	if e.String() != "标" {
		t.Errorf("after backspace ×2 = %q, want 标", e.String())
	}
	// Cursor movement + insertion in the middle.
	e.SetString("ab")
	e.Handle(Key{Kind: KeyLeft})
	e.Handle(Key{KeyRune, 'X'})
	if e.String() != "aXb" {
		t.Errorf("mid-insert = %q, want aXb", e.String())
	}
	e.Handle(Key{Kind: KeyHome})
	e.Handle(Key{Kind: KeyDelete})
	if e.String() != "Xb" {
		t.Errorf("home+delete = %q, want Xb", e.String())
	}
	before, at, after := e.Split()
	if before != "" || at != "X" || after != "b" {
		t.Errorf("Split = %q %q %q", before, at, after)
	}
	// Editing keys are consumed; mode keys are not.
	if e.Handle(Key{Kind: KeyEnter}) || e.Handle(Key{Kind: KeyEsc}) {
		t.Error("enter/esc must be left for the caller's mode logic")
	}
}
