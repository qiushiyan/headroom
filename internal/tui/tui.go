// Package tui holds the raw-terminal primitives for the select picker:
// enter/leave raw mode and turn key bytes into semantic events. Rendering
// stays with the caller.
package tui

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/term"
)

type Event int

const (
	EventUp Event = iota
	EventDown
	EventSelect
	EventCancel
)

// DecodeKeys turns one raw read chunk into events. Arrow keys arrive as a
// whole ESC-[-X sequence in a single read; a lone trailing ESC is a cancel.
func DecodeKeys(buf []byte) []Event {
	var evs []Event
	for i := 0; i < len(buf); i++ {
		switch buf[i] {
		case 0x1b:
			if i+2 < len(buf) && (buf[i+1] == '[' || buf[i+1] == 'O') {
				switch buf[i+2] {
				case 'A':
					evs = append(evs, EventUp)
				case 'B':
					evs = append(evs, EventDown)
				}
				i += 2
			} else if i == len(buf)-1 {
				evs = append(evs, EventCancel)
			}
		case 'k':
			evs = append(evs, EventUp)
		case 'j':
			evs = append(evs, EventDown)
		case '\r', '\n':
			evs = append(evs, EventSelect)
		case 'q', 0x03, 0x04: // q, ctrl-c, ctrl-d
			evs = append(evs, EventCancel)
		}
	}
	return evs
}

type Terminal struct {
	fd     int
	old    *term.State
	events chan Event
	once   sync.Once
}

// Open puts stdin into raw mode and hides the cursor. Always pair with
// Close, including on early error paths.
func Open() (*Terminal, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	t := &Terminal{fd: fd, old: old, events: make(chan Event, 8)}
	fmt.Print("\x1b[?25l")
	go t.readLoop()
	return t, nil
}

func (t *Terminal) Events() <-chan Event { return t.events }

func (t *Terminal) Close() {
	t.once.Do(func() {
		fmt.Print("\x1b[?25h")
		term.Restore(t.fd, t.old)
	})
}

func (t *Terminal) readLoop() {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, ev := range DecodeKeys(buf[:n]) {
			t.events <- ev
		}
	}
}
