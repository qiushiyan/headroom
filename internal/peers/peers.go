// Package peers is how one machine's headroom reads another's: the document
// `headroom readings` prints, and the pull that asks for it over ssh and
// keeps the answer in the store.
//
// Opt-in by configuration alone: a scope with no peers (HEADROOM_PEERS unset)
// pulls nothing, and a reader consults the readings of the peers its scope
// names and no others.
package peers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// Schema is the readings document this binary prints and reads. Two machines
// run their own builds, installed at different times, so the reader states
// what it understands: a document of another schema is refused by name, never
// read around.
const Schema = 1

// Document is what `headroom readings` prints: this machine's own newest
// reading of each of the vendor's accounts, raw as the vendor sent it, so the
// vendor's usage parser on the machine that pulls it stays its only reader.
type Document struct {
	Schema   int       `json:"schema"`
	Vendor   string    `json:"vendor"`
	Readings []Reading `json:"readings"`
}

// Reading is one account's: the identity the ledger keys on, the account's
// name for a person reading the document (nothing matches on it), when the
// response arrived, and the response.
type Reading struct {
	Account     string          `json:"account"`
	Name        string          `json:"name"`
	FetchedAtMS int64           `json:"fetched_at_ms"`
	Body        json.RawMessage `json:"body"`
}

// Encode is the document for this machine's own readings.
func Encode(vendor config.Vendor, own []accountstate.OwnReading) Document {
	doc := Document{Schema: Schema, Vendor: string(vendor), Readings: []Reading{}}
	for _, r := range own {
		doc.Readings = append(doc.Readings, Reading{Account: r.Key.ID(), Name: r.Name, FetchedAtMS: r.Obs.FetchedAtMS, Body: r.Obs.Body})
	}
	return doc
}

// Decode reads another machine's answer for vendor: its readings, by key.
// Whether a reading is one of this machine's accounts, and whether its body
// parses, is decided where it is read (accountstate.PeerObservation), by the
// same check this machine's own bodies pass.
func Decode(vendor config.Vendor, data []byte) (map[string]state.Observation, error) {
	var doc struct {
		Schema   int        `json:"schema"`
		Vendor   string     `json:"vendor"`
		Readings *[]Reading `json:"readings"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, errors.New("its answer is not a readings document")
	}
	switch {
	case doc.Schema != Schema:
		return nil, fmt.Errorf("its headroom prints readings schema %d and this one reads %d — install the same build on both", doc.Schema, Schema)
	case doc.Vendor != string(vendor):
		return nil, fmt.Errorf("it answered for %q when %q was asked", doc.Vendor, vendor)
	case doc.Readings == nil:
		// An empty list is an answer; no list is a document cut short.
		return nil, errors.New("its answer carries no readings list")
	}
	out := make(map[string]state.Observation, len(*doc.Readings))
	for _, r := range *doc.Readings {
		if prev, ok := out[r.Account]; ok && prev.FetchedAtMS >= r.FetchedAtMS {
			continue
		}
		out[r.Account] = state.Observation{FetchedAtMS: r.FetchedAtMS, Body: r.Body}
	}
	return out, nil
}

// Asker asks one machine for its readings of one vendor's accounts: the
// document its headroom printed, or why there is none. SSH in production.
type Asker func(ctx context.Context, peer string, vendor config.Vendor) ([]byte, error)

// askWait bounds the ask, from the connection to the answer read; a child left
// holding the output pipe has a second more to let go (SSH's WaitDelay). A
// pull runs beside a round's requests and the round is not over until it is,
// so a machine slow to answer holds the round about as long as a slow request
// does; the store's lock waits are the store's own.
const askWait = 8 * time.Second

// Pull asks peer for its readings when the store says a pull is due, and
// records what came of it. answered reports that an answer was recorded. err
// is this machine's own bookkeeping failing — the claim or the record could
// not be written — never the other machine's failure to answer, which is
// recorded as its outcome. A pull cut short by the caller's own end records
// nothing: that is not the machine failing to answer.
func Pull(ctx context.Context, st *state.Store, peer string, ask Asker) (answered bool, err error) {
	claim, err := st.ClaimPull(peer, time.Now())
	if err != nil {
		return false, fmt.Errorf("a pull from %s could not be claimed: %w", peer, err)
	}
	if !claim.Permit {
		return false, nil
	}
	actx, cancel := context.WithTimeout(ctx, askWait)
	defer cancel()
	data, askErr := ask(actx, peer, st.Vendor())
	if ctx.Err() != nil {
		return false, nil
	}
	var readings map[string]state.Observation
	if askErr == nil {
		readings, askErr = Decode(st.Vendor(), data)
	}
	failure := ""
	if askErr != nil {
		failure = askErr.Error()
	}
	if err := st.RecordPull(peer, claim.Generation, readings, failure, time.Now()); err != nil {
		return false, fmt.Errorf("what %s said could not be recorded: %w", peer, err)
	}
	return failure == "", nil
}

// answerLimit bounds what a pull reads from the other machine: a reading per
// account, each a few kilobytes.
const answerLimit = 4 << 20

// remoteCommand runs the other machine's headroom with its installed binary
// first on PATH: `make install` puts it in ~/.local/bin, which a
// non-interactive ssh session's PATH often lacks.
func remoteCommand(vendor config.Vendor) string {
	return fmt.Sprintf(`PATH="$HOME/.local/bin:$PATH" exec headroom readings --vendor %s`, vendor)
}

// SSH asks over ssh, as the user's own ssh configuration reaches peer. It
// never prompts — a host key or password that needs a person fails the pull —
// and gives up connecting after three seconds.
func SSH(ctx context.Context, peer string, vendor config.Vendor) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-T", peer, remoteCommand(vendor))
	// A child left holding the output pipe must not outlive the bound.
	cmd.WaitDelay = time.Second
	out, errOut := &capped{limit: answerLimit}, &capped{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, errOut
	err := cmd.Run()
	if out.over {
		return nil, fmt.Errorf("its answer ran past %d bytes", answerLimit)
	}
	if err == nil {
		return out.buf.Bytes(), nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return nil, fmt.Errorf("ssh did not run: %v", err)
	}
	// ssh's own complaint ends its stderr; headroom's error leads its own,
	// above the usage lines.
	lines := strings.Split(strings.TrimSpace(errOut.buf.String()), "\n")
	first, last := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[len(lines)-1])
	if first == "" {
		first, last = "nothing said", "nothing said"
	}
	switch code := exit.ExitCode(); {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("no answer within %s", askWait)
	case code == 255:
		return nil, fmt.Errorf("ssh could not reach it: %s", last)
	case code == 127:
		return nil, errors.New("no headroom on its PATH or in ~/.local/bin")
	case code == 2 && out.buf.Len() == 0 && strings.Contains(first, "unexpected argument readings"):
		// A build from before this command refuses the word as its grammar
		// refuses any unknown one: exit 2, nothing on stdout, and this line.
		return nil, errors.New("its headroom has no readings command — install this build there")
	default:
		return nil, fmt.Errorf("its headroom exited %d: %s", code, first)
	}
}

// capped keeps at most limit bytes and says when more came. The buffer is a
// field, not embedded: an embedded bytes.Buffer would lend capped its
// ReadFrom, and io.Copy — what exec feeds a pipe through — would read the
// whole stream past Write and its limit.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.over = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}
