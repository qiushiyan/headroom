package tui

import (
	"slices"
	"testing"
)

func TestDecodeKeys(t *testing.T) {
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
		{"\x1b", []Event{EventCancel}}, // lone ESC
		{"\x1b[C", nil},                // right arrow: ignored, consumed
		{"jjk", []Event{EventDown, EventDown, EventUp}},
		{"x", nil},
	}
	for _, c := range cases {
		if got := DecodeKeys([]byte(c.in)); !slices.Equal(got, c.want) {
			t.Errorf("DecodeKeys(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
