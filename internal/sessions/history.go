package sessions

import (
	"bufio"
	"encoding/json"
	"io"
)

// History is one account's prompt history distilled to attribution facts:
// for each session id, when this account last drove it.
type History struct {
	Newest map[string]int64 // session id → newest prompt timestamp (ms)
	Lines  int              // complete lines seen
	Bad    int              // complete lines that failed the contract
}

// ParseHistory reads one account's history.jsonl. Each line records a prompt
// as {sessionId, timestamp, …}; the decoded field is compared, never a
// substring — prompt bodies quote other sessions' UUIDs (verified in the
// live store, including one cross-account), and a substring match would
// re-home a session to whoever pasted its id.
//
// A line that doesn't parse, or parses without both fields, counts Bad and
// contributes nothing: a torn final line during a concurrent write is
// ordinary, and inventing a claim from half a record is how attribution
// would silently rot. An empty or missing file is an empty History — a
// fresh account has no history and that is not evidence of anything.
func ParseHistory(r io.Reader) History {
	h := History{Newest: map[string]int64{}}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return h // a partial final line is a write in progress, not a claim
		}
		if len(line) <= 1 {
			continue
		}
		h.Lines++
		var rec struct {
			SessionID string `json:"sessionId"`
			Timestamp int64  `json:"timestamp"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.SessionID == "" || rec.Timestamp <= 0 {
			h.Bad++
		} else if rec.Timestamp > h.Newest[rec.SessionID] {
			h.Newest[rec.SessionID] = rec.Timestamp
		}
	}
}
