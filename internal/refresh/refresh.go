// Package refresh owns one request lifecycle: local eligibility, durable claim,
// fetch, interpretation and completion. It never retries a request.
package refresh

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Candidate contains spendable credentials, never permission to fetch.
// Only Start can turn a candidate into a durable claim.
//
// A candidate binds everything one request is made of — whose budget it
// spends, the vendor whose endpoint it asks, the prepared request and the
// token it carries. The fields stay unexported so a caller cannot pair one
// account's key with another's token or header.
type Candidate struct {
	key     state.Key
	vendor  config.Vendor
	url     string
	headers [][2]string // beyond Authorization
	dir     string      // what the 401 re-read is asked about
	token   string

	// What the response must not contradict (usage.Reading.BelongsTo).
	accountID, userID string
}

// userAgent identifies headroom to the Codex backend. The vendor's own client
// sends one; headroom says who it is rather than borrowing theirs.
const userAgent = "headroom (+https://github.com/qiushiyan/headroom)"

// PrepareCodex is the Codex half of the access reader: the eligibility table
// (rows 5–7) over the auth snapshot the account already carries. It reads no
// file, spawns nothing, and is only reached once health is OK. The request is
// bound here, once — the account header, the token and the ledger key all come
// from the same snapshot, so a caller cannot mismatch them.
func PrepareCodex(a accounts.Account, now time.Time) (*Candidate, accountstate.AttemptState) {
	snap := a.Auth
	switch {
	case snap.State != codexauth.OK:
		return nil, accountstate.AttemptIdentityUnknown
	case snap.TokenStale(now.Unix()):
		return nil, accountstate.AttemptTokenStale
	}
	return &Candidate{
		key:       state.Key{UUID: snap.Identity(), Name: a.Name},
		vendor:    config.Codex,
		url:       a.Scope.UsageURL,
		headers:   [][2]string{{"ChatGPT-Account-Id", snap.AccountID}, {"User-Agent", userAgent}},
		dir:       a.Dir(),
		token:     snap.AccessToken,
		accountID: snap.AccountID,
		userID:    snap.UserID,
	}, accountstate.AttemptPending
}

func Prepare(a accounts.Account, blob creds.Blob, readable bool, now time.Time) (*Candidate, accountstate.AttemptState) {
	switch {
	case !readable:
		return nil, accountstate.AttemptCredentialUnreadable
	case !blob.TokenUsable(now.UnixMilli()):
		return nil, accountstate.AttemptTokenStale
	case !a.Readable:
		return nil, accountstate.AttemptIdentityUnknown
	default:
		return &Candidate{
			key:    state.Key{UUID: a.AccountID, Name: a.Name},
			vendor: a.Scope.Vendor,
			url:    a.Scope.UsageURL,
			dir:    a.ConfigDir,
			token:  blob.Token,
		}, accountstate.AttemptPending
	}
}

// Reread samples the credential a rejected request was made with, through the
// vendor's own credential reader. Its answer annotates the attempt and
// nothing else.
type Reread func(dir string) (token string, ok bool)

type TokenEvidence int

const (
	TokenUnknown TokenEvidence = iota
	TokenUnchanged
	TokenChanged
)

type Result struct {
	Index         int
	Attempt       accountstate.Attempt
	Observation   *accountstate.Observation
	StoreErr      error // claim/completion failure, independent of received data
	TokenAfter401 TokenEvidence
}

func interpret(c Candidate, res response, at time.Time) (Result, state.Outcome, []byte) {
	r := Result{Attempt: accountstate.Attempt{HTTPCode: res.StatusCode}}
	switch {
	case res.Err != nil:
		r.Attempt.State = accountstate.AttemptTransport
		return r, state.OutcomeFailed, nil
	case res.StatusCode == http.StatusTooManyRequests:
		r.Attempt.State = accountstate.AttemptRefused
		return r, state.OutcomeRefused, nil
	case res.StatusCode != http.StatusOK:
		r.Attempt.State = accountstate.AttemptHTTP
		return r, state.OutcomeFailed, nil
	}
	// A body that names another account is recorded as unparseable and never
	// shown or stored: whose quota was measured is established by decoded
	// identity, never by which row the request was started from.
	reading, err := usage.Parse(c.vendor, res.Body)
	if err != nil || !reading.BelongsTo(c.accountID, c.userID) {
		r.Attempt.State = accountstate.AttemptUnparseable
		return r, state.OutcomeSpent, nil
	}
	r.Attempt.State = accountstate.AttemptOK
	if len(reading.Rows) == 0 {
		r.Attempt.State = accountstate.AttemptNoLimits
	}
	r.Observation = accountstate.FromReading(reading, at.Unix(), accountstate.SourceLive)
	return r, state.OutcomeStored, res.Body
}

// Start returns one result per non-nil candidate and closes after all workers
// finish. Each result addresses this input slice; callers finish draining a
// round before rediscovering accounts. Workers never mutate caller facts.
// reread optionally samples a rejected token for diagnostic callers.
//
// The store is scoped to one vendor, and a candidate of the other vendor is
// refused before any claim: its key would be spent in the wrong ledger and
// its body stored where the wrong parser replays it.
func Start(ctx context.Context, st *state.Store, candidates []*Candidate, reread Reread) <-chan Result {
	updates := make(chan Result, len(candidates))
	keys := make([]state.Key, 0, len(candidates))
	indices := make([]int, 0, len(candidates))
	for i, c := range candidates {
		if c == nil {
			continue
		}
		if c.vendor != st.Vendor() {
			updates <- Result{Index: i, Attempt: accountstate.Attempt{State: accountstate.AttemptStateUnavailable},
				StoreErr: fmt.Errorf("a %s request cannot be claimed against the %s store", c.vendor, st.Vendor())}
			continue
		}
		keys = append(keys, c.key)
		indices = append(indices, i)
	}
	// Nothing to ask for is nothing to claim: the ledger is only ever opened
	// for writing on behalf of a request that could leave.
	var decisions []state.Decision
	var err error
	if len(keys) > 0 {
		decisions, err = st.Claim(keys, time.Now())
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	for j, dec := range decisions {
		i := indices[j]
		if !dec.Permit {
			r := Result{Index: i, Attempt: accountstate.Attempt{State: accountstate.AttemptDeferred, NextEligibleAt: dec.NextEligible.Unix()}, StoreErr: err}
			if err != nil || dec.Degraded {
				r.Attempt.State = accountstate.AttemptStateUnavailable
				if r.StoreErr == nil {
					r.StoreErr = state.ErrCorrupt
				}
			}
			updates <- r
			continue
		}
		c := *candidates[i]
		wg.Add(1)
		go func(i int, c Candidate, generation int64) {
			defer wg.Done()
			res := fetch(ctx, client, c)
			at := time.Now()
			r, outcome, body := interpret(c, res, at)
			r.Index = i
			if res.StatusCode == http.StatusUnauthorized && reread != nil {
				token, ok := reread(c.dir)
				switch {
				case !ok:
					r.TokenAfter401 = TokenUnknown
				case token != c.token:
					r.TokenAfter401 = TokenChanged
				default:
					r.TokenAfter401 = TokenUnchanged
				}
			}
			if ctx.Err() == nil {
				next, err := st.Complete(c.key, generation, outcome, body, at)
				r.StoreErr = err
				if !next.IsZero() {
					r.Attempt.NextEligibleAt = next.Unix()
				}
			}
			updates <- r
		}(i, c, dec.Generation)
	}
	go func() { wg.Wait(); close(updates) }()
	return updates
}
