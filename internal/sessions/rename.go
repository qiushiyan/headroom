package sessions

import (
	"encoding/json"
	"os"
)

// AppendCustomTitle renames a session the way the vendor does it: one
// custom-title record appended to the transcript, which every title reader
// (Claude Code's picker included) ranks above the generated ai-title. This
// is one of exactly two mutations headroom makes to vendor state — the
// caller must refuse it while the session is open elsewhere, because the
// vendor's own writer may hold the file.
func AppendCustomTitle(path, id, title string) error {
	line, err := json.Marshal(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", title, id})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	// One Write for the whole line: an append either lands whole or not at
	// all from any reader's point of view.
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
