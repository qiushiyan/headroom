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
		// Home and End have three spellings each across terminals: the bare
		// final, and two numbered tilde forms (VT220 and rxvt).
		{"\x1b[F", []Key{{Kind: KeyEnd}}},
		{"\x1b[1~", []Key{{Kind: KeyHome}}},
		{"\x1b[7~", []Key{{Kind: KeyHome}}},
		{"\x1b[4~", []Key{{Kind: KeyEnd}}},
		{"\x1b[8~", []Key{{Kind: KeyEnd}}},
		// A byte no CSI sequence may hold ends the sequence unrecognized, and
		// decoding resumes with the next key rather than swallowing it.
		{"\x1b[\x01j", []Key{{KeyRune, 'j'}}},
		{"k", []Key{{KeyRune, 'k'}}},
		{"j", []Key{{KeyRune, 'j'}}},
		{"/", []Key{{KeyRune, '/'}}},
		{" ", []Key{{KeyRune, ' '}}},
		{"\r", []Key{{Kind: KeyEnter}}},
		{"\n", []Key{{Kind: KeyEnter}}},
		{"\t", []Key{{Kind: KeyTab}}},
		{"\x7f", []Key{{Kind: KeyBackspace}}},
		{"\x08", []Key{{Kind: KeyBackspace}}}, // ^H: what some terminals send for backspace
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
	// An escape arriving mid-rune abandons the fragment: the sequence decodes,
	// and the next rune is not glued to the dead bytes.
	d = Decoder{}
	got = append(feed(&d, string(b[:1]), "\x1b[A"), d.Feed([]byte("é"))...)
	if want := []Key{{Kind: KeyUp}, {KeyRune, 'é'}}; !slices.Equal(got, want) {
		t.Errorf("ESC mid-rune = %v, want %v", got, want)
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
