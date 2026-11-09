package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// loggedLaunch is one line of the launch log as a person reads it.
type loggedLaunch struct {
	V       int    `json:"v"`
	At      string `json:"at"`
	Vendor  string `json:"vendor"`
	Mode    string `json:"mode"`
	Reason  string `json:"reason"`
	Chosen  string `json:"chosen"`
	CWD     string `json:"cwd"`
	Session string `json:"session"`
	PID     int    `json:"pid"`
	Problem string `json:"problem"`
}

func launchLogPath(accountsRoot string) string { return filepath.Join(accountsRoot, "launches.jsonl") }

// readLaunchLog returns every line, oldest first, and how many did not parse.
func readLaunchLog(accountsRoot string) ([]loggedLaunch, int, error) {
	f, err := os.Open(launchLogPath(accountsRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []loggedLaunch
	skipped := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l loggedLaunch
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.At == "" {
			skipped++
			continue
		}
		out = append(out, l)
	}
	return out, skipped, sc.Err()
}

// dryRow is one account's row of `headroom launch --dry-run`'s table, split
// after the account's name: session, week, resets, busy, pending, load, then
// the note's words. nil when the table has no row for it.
func dryRow(table, name string) []string {
	for line := range strings.SplitSeq(table, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && (f[0] == "→" || f[0] == "·") {
			f = f[1:]
		}
		if len(f) > 6 && f[0] == name {
			return f[1:]
		}
	}
	return nil
}

// dryNote is the note column of an account's dry-run row.
func dryNote(table, name string) string {
	if r := dryRow(table, name); len(r) > 6 {
		return strings.Join(r[6:], " ")
	}
	return ""
}
