package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A transcript whose last write was torn — the vendor died mid-append — still
// takes a rename: the record lands on a line of its own, never glued to the
// torn one, where no reader would find it.
func TestARenameAfterATornLineStillTakes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	torn := `{"type":"ai-title","aiTitle":"t","sessionId":"s"}` + "\n" + `{"type":"assistant","message":{"content":"tor`
	if err := os.WriteFile(path, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendCustomTitle(path, "s", "renamed"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), torn) {
		t.Errorf("the rename rewrote what was there: %q", data)
	}
	if got := ParseTail(data, "", true).CustomTitle; got != "renamed" {
		t.Errorf("custom title = %q after a torn line, want the rename", got)
	}
}
