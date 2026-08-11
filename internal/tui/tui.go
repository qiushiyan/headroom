// Package tui owns the terminal session for the interactive surfaces: raw-mode
// lifetime — including restoration when the process is killed mid-session —
// alternate-screen entry and exit, and turning key bytes into decoded keys.
// Keys are physical, not semantic: the decoder says "the user pressed j",
// and each surface owns what j means there. Rendering stays with the caller.
package tui

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// KeyKind names a physical key. Printable input arrives as KeyRune; control
// chords as KeyCtrl with the letter in Rune. Semantics live in the surfaces.
type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyDelete
	KeyEnter
	KeyEsc
	KeyTab
	KeyBackspace
	KeyCtrl // Rune holds the lowercase letter: ctrl-c is Key{KeyCtrl, 'c'}
)

type Key struct {
	Kind KeyKind
	Rune rune
}

// escTimeout is the pause that resolves a bare ESC as the Escape key rather
// than the start of a sequence still in flight.
const escTimeout = 50 * time.Millisecond

// Decoder turns raw reads into keys, carrying incomplete escape sequences and
// partial UTF-8 runes across read boundaries — a terminal may split either
// anywhere.
type Decoder struct {
	esc []byte // prefix of an unfinished escape sequence
	seq []byte // params of a CSI sequence in flight (after ESC [)
	utf []byte // prefix of an unfinished multi-byte rune
}

// Pending reports whether an escape prefix is held; when true and input
// pauses, call Flush — only the pause distinguishes a lone ESC from a split
// sequence.
func (d *Decoder) Pending() bool { return len(d.esc) > 0 }

func (d *Decoder) Feed(buf []byte) []Key {
	var keys []Key
	for _, b := range buf {
		keys = d.feedByte(keys, b)
	}
	return keys
}

func (d *Decoder) feedByte(keys []Key, b byte) []Key {
	// A rune split across reads reassembles before anything else looks at
	// the byte; an escape byte arriving mid-rune abandons the fragment.
	if len(d.utf) > 0 {
		if b >= 0x80 && b < 0xc0 {
			d.utf = append(d.utf, b)
			if r, _ := utf8.DecodeRune(d.utf); r != utf8.RuneError {
				keys = append(keys, Key{KeyRune, r})
				d.utf = d.utf[:0]
			} else if !utf8.FullRune(d.utf) && len(d.utf) < utf8.UTFMax {
				return keys // still incomplete
			} else {
				d.utf = d.utf[:0] // invalid; drop
			}
			return keys
		}
		d.utf = d.utf[:0]
	}

	switch len(d.esc) {
	case 0:
		return d.plainByte(keys, b)
	case 1: // held: ESC
		if b == '[' || b == 'O' {
			d.esc = append(d.esc, b)
			d.seq = d.seq[:0]
			return keys
		}
		keys = append(keys, Key{Kind: KeyEsc}) // the held ESC was alone
		d.esc = d.esc[:0]
		return d.feedByte(keys, b)
	default: // held: ESC + [ or O — parameters until a final byte
		if d.esc[1] == 'O' {
			keys = appendSpecial(keys, b, nil)
			d.esc = d.esc[:0]
			return keys
		}
		// CSI: parameter/intermediate bytes 0x20–0x3f accumulate; a final
		// byte 0x40–0x7e ends the sequence.
		if b >= 0x40 && b <= 0x7e {
			keys = appendSpecial(keys, b, d.seq)
			d.esc, d.seq = d.esc[:0], d.seq[:0]
			return keys
		}
		if b >= 0x20 && b <= 0x3f {
			d.seq = append(d.seq, b)
			return keys
		}
		d.esc, d.seq = d.esc[:0], d.seq[:0] // malformed; drop
		return keys
	}
}

func (d *Decoder) plainByte(keys []Key, b byte) []Key {
	switch {
	case b == 0x1b:
		d.esc = append(d.esc, b)
	case b == '\r' || b == '\n':
		keys = append(keys, Key{Kind: KeyEnter})
	case b == '\t':
		keys = append(keys, Key{Kind: KeyTab})
	case b == 0x7f || b == 0x08:
		keys = append(keys, Key{Kind: KeyBackspace})
	case b < 0x20:
		keys = append(keys, Key{KeyCtrl, rune('a' + b - 1)})
	case b < 0x80:
		keys = append(keys, Key{KeyRune, rune(b)})
	default:
		d.utf = append(d.utf, b)
	}
	return keys
}

// appendSpecial resolves a CSI/SS3 final byte, with any parameter bytes seen.
func appendSpecial(keys []Key, final byte, params []byte) []Key {
	switch final {
	case 'A':
		return append(keys, Key{Kind: KeyUp})
	case 'B':
		return append(keys, Key{Kind: KeyDown})
	case 'C':
		return append(keys, Key{Kind: KeyRight})
	case 'D':
		return append(keys, Key{Kind: KeyLeft})
	case 'H':
		return append(keys, Key{Kind: KeyHome})
	case 'F':
		return append(keys, Key{Kind: KeyEnd})
	case '~':
		switch string(params) {
		case "1", "7":
			return append(keys, Key{Kind: KeyHome})
		case "3":
			return append(keys, Key{Kind: KeyDelete})
		case "4", "8":
			return append(keys, Key{Kind: KeyEnd})
		case "5":
			return append(keys, Key{Kind: KeyPgUp})
		case "6":
			return append(keys, Key{Kind: KeyPgDn})
		}
	}
	return keys // unrecognized: consumed, ignored
}

// Flush resolves a held prefix once input has paused: a bare ESC (or a
// dangling sequence start) is the Escape key. A dangling rune fragment is
// dropped — half a rune has no honest keypress.
func (d *Decoder) Flush() []Key {
	d.utf = d.utf[:0]
	if len(d.esc) == 0 {
		return nil
	}
	d.esc, d.seq = d.esc[:0], d.seq[:0]
	return []Key{{Kind: KeyEsc}}
}

// LineEdit is the shared single-line editor under rename and search input:
// a rune buffer with a cursor, fed keys, rendered by the caller. It exists
// once because backspace over a multi-byte rune and cursor-vs-display-column
// arithmetic are exactly the mistakes worth making only once.
type LineEdit struct {
	runes []rune
	cur   int
}

func (e *LineEdit) String() string { return string(e.runes) }

func (e *LineEdit) SetString(s string) {
	e.runes = []rune(s)
	e.cur = len(e.runes)
}

// Split returns the text before the cursor, the rune under it (" " when the
// cursor sits at the end), and the text after — the three spans a caller
// needs to render an inverse-video cursor.
func (e *LineEdit) Split() (before, at, after string) {
	if e.cur >= len(e.runes) {
		return string(e.runes), " ", ""
	}
	return string(e.runes[:e.cur]), string(e.runes[e.cur]), string(e.runes[e.cur+1:])
}

// Handle applies one key; false means the key isn't an editing key and the
// caller's mode logic should interpret it (enter, esc, …).
func (e *LineEdit) Handle(k Key) bool {
	switch k.Kind {
	case KeyRune:
		e.runes = append(e.runes[:e.cur], append([]rune{k.Rune}, e.runes[e.cur:]...)...)
		e.cur++
	case KeyBackspace:
		if e.cur > 0 {
			e.runes = append(e.runes[:e.cur-1], e.runes[e.cur:]...)
			e.cur--
		}
	case KeyDelete:
		if e.cur < len(e.runes) {
			e.runes = append(e.runes[:e.cur], e.runes[e.cur+1:]...)
		}
	case KeyLeft:
		if e.cur > 0 {
			e.cur--
		}
	case KeyRight:
		if e.cur < len(e.runes) {
			e.cur++
		}
	case KeyHome:
		e.cur = 0
	case KeyEnd:
		e.cur = len(e.runes)
	default:
		return false
	}
	return true
}

type Terminal struct {
	in      *os.File
	out     *os.File
	ownsTTY bool
	alt     bool
	old     *term.State
	events  chan Key
	sigs    chan os.Signal
	once    sync.Once

	mu       sync.Mutex
	epilogue func()
}

// Open puts stdin into raw mode and hides the cursor, rendering to stdout —
// the shape select and watch always had. The session owns its own
// restoration: Close is idempotent, and SIGTERM/SIGHUP/SIGINT restore the
// terminal before the process dies. (Raw mode disables ISIG, so ctrl-c
// arrives as a key byte — these signals only come from outside.)
func Open() (*Terminal, error) {
	return open(os.Stdin, os.Stdout, false, false)
}

// OpenTTY runs the session on /dev/tty — both keys and frames — leaving
// stdout free to carry a machine-readable result, and uses the alternate
// screen so the session vanishes from scrollback on exit. Alt-screen entry
// and exit live inside the same once-guarded Close and the same signal path
// as raw mode: split from them, a SIGTERM could restore cooked mode yet
// strand the user in the alternate buffer.
func OpenTTY() (*Terminal, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("no controlling terminal: %w", err)
	}
	t, err := open(tty, tty, true, true)
	if err != nil {
		tty.Close()
		return nil, err
	}
	return t, nil
}

func open(in, out *os.File, ownsTTY, alt bool) (*Terminal, error) {
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	t := &Terminal{in: in, out: out, ownsTTY: ownsTTY, alt: alt, old: old,
		events: make(chan Key, 8), sigs: make(chan os.Signal, 1)}
	if alt {
		fmt.Fprint(out, "\x1b[?1049h")
	}
	fmt.Fprint(out, "\x1b[?25l")
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

func (t *Terminal) Events() <-chan Key { return t.events }

// Out is where frames belong: stdout for Open, /dev/tty for OpenTTY.
func (t *Terminal) Out() *os.File { return t.out }

// Size is the terminal's current dimensions, from the same descriptor frames
// go to.
func (t *Terminal) Size() (w, h int, err error) {
	return term.GetSize(int(t.out.Fd()))
}

// OnClose registers one function to run first inside Close — still in raw
// mode, before the cursor returns and cooked mode is restored. It exists for
// the same reason alt-screen exit lives inside Close: a surface's final
// rendering step (the board stepping below its last, newline-less line) must
// share the once-guard and the signal path, or a SIGTERM would restore the
// terminal yet leave the shell's next prompt glued to the frame.
func (t *Terminal) OnClose(f func()) {
	t.mu.Lock()
	t.epilogue = f
	t.mu.Unlock()
}

func (t *Terminal) Close() {
	t.once.Do(func() {
		t.mu.Lock()
		epilogue := t.epilogue
		t.mu.Unlock()
		if epilogue != nil {
			epilogue()
		}
		signal.Stop(t.sigs)
		close(t.sigs)
		fmt.Fprint(t.out, "\x1b[?25h")
		if t.alt {
			fmt.Fprint(t.out, "\x1b[?1049l")
		}
		term.Restore(int(t.in.Fd()), t.old)
		if t.ownsTTY {
			t.in.Close()
		}
	})
}

func (t *Terminal) readLoop() {
	chunks := make(chan []byte)
	go func() {
		for {
			buf := make([]byte, 64)
			n, err := t.in.Read(buf)
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
			for _, k := range d.Feed(chunk) {
				t.events <- k
			}
		case <-pause:
			for _, k := range d.Flush() {
				t.events <- k
			}
		}
	}
}
