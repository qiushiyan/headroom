package sessions

import (
	"encoding/json"
	"os"
)

// AppendCustomTitle renames a session the way the vendor does it: one
// custom-title record appended to the transcript, which every title reader
// (Claude Code's picker included) ranks above the generated ai-title. It is
// one of the few documented mutations headroom makes to vendor state (with
// DeleteTranscript and accounts remove's Keychain delete) — the caller must
// refuse it while the session is open elsewhere, because the vendor's own
// writer may hold the file.
func AppendCustomTitle(path, id, title string) error {
	line, err := json.Marshal(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", title, id})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	// A transcript whose last write was torn (its writer died mid-append)
	// would swallow the record into that line; a newline first closes it.
	line = append(line, '\n')
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	// One Write for the whole line: an append either lands whole or not at
	// all from any reader's point of view.
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
