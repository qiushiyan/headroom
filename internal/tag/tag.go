// Package tag carries the one vocabulary headroom uses for degraded vendor
// data: a field is either good, legitimately absent, or present and no longer
// parseable. Keeping the three states in one place is what lets a percent, a
// timestamp and a credential expiry all degrade the same way — and lets
// `--json` name them once for every consumer.
package tag

// State distinguishes absent from broken. That distinction is the whole
// point: an untouched limit window has no reset timestamp and that is fine,
// while a reset timestamp that no longer parses is drift the renderer must
// not quietly round to zero.
type State uint8

const (
	OK   State = iota
	None       // legitimately absent
	Bad        // present but no longer parses — shape drift
)

// Name is the wire spelling used by --json.
func (s State) Name() string {
	switch s {
	case None:
		return "none"
	case Bad:
		return "bad"
	default:
		return "ok"
	}
}
