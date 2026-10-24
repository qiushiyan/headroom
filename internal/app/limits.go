package app

// The limits surface: the board's document without the board's costs. A
// machine consumer — a statusline refresher asking about one account a few
// times a minute — cannot afford the board's `--json`, which probes health
// for every account (~170ms per `claude auth status`, the dominant term) and
// may spend every account's request budget to answer about one. This surface
// reads only what is already on disk: discovery, the current marker, and the
// newest stored observation. No health probe, no Keychain read, no claim, no
// request — it is not a second door onto the endpoint because it never
// touches the door at all. Refreshing what is known remains the fetching
// surfaces' job (`headroom --json`, the board, `check`), all behind the same
// claim.

import (
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
)

// Limits is what is already known about limits, as the board's JSON
// document, read from disk alone: no health probe, no network, never a
// request spent. account, when set, keeps the accounts of that name: the same
// email can be an account of both vendors, and then both answer, each under
// its own vendor.
func Limits(std Streams, scopes []config.Scope, account string) error {
	accountSet := account != ""
	// Every selected vendor, read from disk alone. --account filters by name
	// inside them: the same email can be an account of both vendors, and then
	// both rows answer, each under its own "vendor".
	now := time.Now()
	var boards []round.View
	var selectErr error
	matched := 0
	for _, scope := range scopes {
		r := round.FromDisk(scope, now)
		if accountSet {
			if err := r.Narrow(account); err != nil {
				// The vendor stays in the document — its `current` is still a
				// fact — with no account of that name under it.
				if selectErr == nil {
					selectErr = err
				}
			} else {
				matched++
			}
		}
		boards = append(boards, r.View())
	}
	if accountSet && matched == 0 {
		return selectErr
	}
	data, err := jsonDocument(boards, now)
	if err != nil {
		return err
	}
	_, err = std.Out.Write(append(data, '\n'))
	return err
}
