package render

import (
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accountstate"
)

// Both layouts say what an account is kept for, the block beside its routing
// mark and the compact row among its notes.
func TestAReservationIsSaidOnTheAccountsRow(t *testing.T) {
	v := accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Plan: "max 5x", Next: true, Reserved: "reserved for ~/dev/itell"}
	if got := (Palette{}).headerLine(v); !strings.Contains(got, "reserved for ~/dev/itell  ← next") {
		t.Errorf("block: %q", got)
	}
	if got := (Palette{}).caption(v, 0, false); !strings.Contains(got, "reserved for ~/dev/itell") {
		t.Errorf("compact: %q", got)
	}
	v.Reserved = ""
	if got := (Palette{}).headerLine(v); strings.Contains(got, "reserved") {
		t.Errorf("no reservation: %q", got)
	}
}
