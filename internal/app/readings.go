package app

// The readings surface: what another machine's headroom pulls from this one.
// A machine can ask the usage endpoint only about an account whose access
// token it keeps live, and only a session there keeps it so — so the machine
// driving an account holds its current reading, and another machine reads it
// from there (DESIGN.md § Readings from other machines). Read from disk alone,
// like limits: no health probe, no Keychain read, no request, so it answers
// over ssh even where the login Keychain is sealed.

import (
	"encoding/json"
	"io"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/peers"
	"github.com/qiushiyan/headroom/internal/state"
)

// Readings prints this machine's own newest reading of each of the scope's
// accounts as one readings document. A vendor this machine does not have is
// an empty document, not an error: "nothing known here" is an answer.
func Readings(out io.Writer, scope config.Scope) error {
	set := accounts.Discover(scope)
	doc := peers.Encode(scope.Vendor, accountstate.OwnReadings(set, state.Open(scope).Load(), time.Now()))
	return json.NewEncoder(out).Encode(doc)
}
