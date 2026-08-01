package tui

import (
	"slices"
	"testing"
)

func feed(d *Decoder, chunks ...string) []Event {
	var evs []Event
	for _, c := range chunks {
		evs = append(evs, d.Feed([]byte(c))...)
	}
	return evs
}

func TestDecoderWholeChunks(t *testing.T) {
	cases := []struct {
		in   string
		want []Event
	}{
		{"\x1b[A", []Event{EventUp}},
		{"\x1b[B", []Event{EventDown}},
		{"\x1bOA", []Event{EventUp}}, // application cursor mode
		{"k", []Event{EventUp}},
		{"j", []Event{EventDown}},
		{"\r", []Event{EventSelect}},
		{"\n", []Event{EventSelect}},
		{"q", []Event{EventCancel}},
		{"\x03", []Event{EventCancel}},
		{"\x1b[C", nil}, // right arrow: consumed, ignored
		{"jjk", []Event{EventDown, EventDown, EventUp}},
		{"x", nil},
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
// split of ESC [ A must still decode as one arrow key, not a cancel.
func TestDecoderSplitSequences(t *testing.T) {
	cases := []struct {
		chunks []string
		want   []Event
	}{
		{[]string{"\x1b", "[A"}, []Event{EventUp}},
		{[]string{"\x1b[", "A"}, []Event{EventUp}},
		{[]string{"\x1b", "[", "A"}, []Event{EventUp}},
		{[]string{"\x1b", "[B"}, []Event{EventDown}},
		{[]string{"\x1b", "OA"}, []Event{EventUp}},
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

// A bare ESC is only a cancel once input pauses (Flush), never eagerly.
func TestDecoderLoneEsc(t *testing.T) {
	var d Decoder
	if got := d.Feed([]byte{0x1b}); got != nil {
		t.Errorf("lone ESC decoded eagerly: %v", got)
	}
	if !d.Pending() {
		t.Fatal("ESC should be held pending")
	}
	if got := d.Flush(); !slices.Equal(got, []Event{EventCancel}) {
		t.Errorf("Flush = %v, want cancel", got)
	}
	if d.Pending() {
		t.Error("Flush should clear pending")
	}

	// A dangling sequence start also resolves to cancel on pause.
	d = Decoder{}
	feed(&d, "\x1b[")
	if got := d.Flush(); !slices.Equal(got, []Event{EventCancel}) {
		t.Errorf("dangling ESC[: Flush = %v, want cancel", got)
	}
}

func TestDecoderEscThenOtherKeys(t *testing.T) {
	// ESC followed by a non-sequence byte: the ESC was a real keypress.
	var d Decoder
	if got := feed(&d, "\x1bj"); !slices.Equal(got, []Event{EventCancel, EventDown}) {
		t.Errorf("ESC j = %v", got)
	}
	// ESC ESC: first resolves as cancel, second held for Flush.
	d = Decoder{}
	if got := feed(&d, "\x1b\x1b"); !slices.Equal(got, []Event{EventCancel}) {
		t.Errorf("ESC ESC = %v", got)
	}
	if got := d.Flush(); !slices.Equal(got, []Event{EventCancel}) {
		t.Errorf("ESC ESC Flush = %v", got)
	}
}
