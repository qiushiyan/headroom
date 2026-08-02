// Package tui owns the terminal session for the select picker: raw-mode
// lifetime — including restoration when the process is killed mid-session —
// and turning key bytes into semantic events. Rendering stays with the
// caller.
package tui

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

type Event int

const (
	EventUp Event = iota
	EventDown
	EventSelect
	EventCancel
	EventRefresh
)

// escTimeout is the pause that resolves a bare ESC as the Escape key rather
// than the start of a sequence still in flight.
const escTimeout = 50 * time.Millisecond

// Decoder turns raw reads into events, carrying incomplete escape sequences
// across read boundaries — a terminal may split ESC [ A anywhere.
type Decoder struct {
	pending []byte // prefix of an unfinished escape sequence
}

// Pending reports whether a prefix is held; when true and input pauses,
// call Flush — only the pause distinguishes a lone ESC from a split
// sequence.
func (d *Decoder) Pending() bool { return len(d.pending) > 0 }

func (d *Decoder) Feed(buf []byte) []Event {
	var evs []Event
	for _, b := range buf {
		switch len(d.pending) {
		case 0:
			if b == 0x1b {
				d.pending = append(d.pending, b)
			} else {
				evs = appendPlain(evs, b)
			}
		case 1: // held: ESC
			if b == '[' || b == 'O' {
				d.pending = append(d.pending, b)
			} else {
				evs = append(evs, EventCancel) // the held ESC was alone
				d.pending = d.pending[:0]
				if b == 0x1b {
					d.pending = append(d.pending, b)
				} else {
					evs = appendPlain(evs, b)
				}
			}
		default: // held: ESC + [ or O — b is the final byte
			switch b {
			case 'A':
				evs = append(evs, EventUp)
			case 'B':
				evs = append(evs, EventDown)
			}
			d.pending = d.pending[:0]
		}
	}
	return evs
}

// Flush resolves a held prefix once input has paused: a bare ESC (or a
// dangling sequence start) is a cancel.
func (d *Decoder) Flush() []Event {
	if len(d.pending) == 0 {
		return nil
	}
	d.pending = d.pending[:0]
	return []Event{EventCancel}
}

func appendPlain(evs []Event, b byte) []Event {
	switch b {
	case 'k':
		return append(evs, EventUp)
	case 'j':
		return append(evs, EventDown)
	case '\r', '\n':
		return append(evs, EventSelect)
	case 'q', 0x03, 0x04: // q, ctrl-c, ctrl-d
		return append(evs, EventCancel)
	case 'r':
		return append(evs, EventRefresh)
	}
	return evs
}

type Terminal struct {
	fd     int
	old    *term.State
	events chan Event
	sigs   chan os.Signal
	once   sync.Once
}

// Open puts stdin into raw mode and hides the cursor. The session owns its
// own restoration: Close is idempotent, and SIGTERM/SIGHUP/SIGINT restore
// the terminal before the process dies. (Raw mode disables ISIG, so ctrl-c
// arrives as a key byte — these signals only come from outside.)
func Open() (*Terminal, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	t := &Terminal{fd: fd, old: old, events: make(chan Event, 8), sigs: make(chan os.Signal, 1)}
	fmt.Print("\x1b[?25l")
	signal.Notify(t.sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		sig, ok := <-t.sigs
		if !ok {
			return
		}
		t.Close()
		// Re-raise under the default disposition so the exit status stays
		// honest about the signal death.
		if s, isSig := sig.(syscall.Signal); isSig {
			syscall.Kill(os.Getpid(), s)
		} else {
			os.Exit(1)
		}
	}()
	go t.readLoop()
	return t, nil
}

func (t *Terminal) Events() <-chan Event { return t.events }

func (t *Terminal) Close() {
	t.once.Do(func() {
		signal.Stop(t.sigs)
		close(t.sigs)
		fmt.Print("\x1b[?25h")
		term.Restore(t.fd, t.old)
	})
}

func (t *Terminal) readLoop() {
	chunks := make(chan []byte)
	go func() {
		for {
			buf := make([]byte, 64)
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(chunks)
				return
			}
			chunks <- buf[:n]
		}
	}()
	var d Decoder
	for {
		var pause <-chan time.Time
		if d.Pending() {
			pause = time.After(escTimeout)
		}
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			for _, ev := range d.Feed(chunk) {
				t.events <- ev
			}
		case <-pause:
			for _, ev := range d.Flush() {
				t.events <- ev
			}
		}
	}
}
