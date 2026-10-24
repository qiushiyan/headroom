package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
)

// defaultCleanupPeriodDays is Claude Code's retention when no settings source
// sets cleanupPeriodDays (its documented retention_sweep event; read 2.1.287).
const defaultCleanupPeriodDays = 30

// checkRetention audits the one setting every account applies to the shared
// session store. Claude Code's cleanup sweep deletes transcripts older than
// the cleanupPeriodDays its config dir's settings.json names, and through the
// store link every account's sweep prunes the one shared store — so the store
// keeps only the shortest period any sharing account names, and one account
// left on the default quietly undoes a longer period set everywhere else.
// Only the user settings file varies by account; managed and project settings
// apply to every account alike. An unreadable settings.json is untested, not
// failed: Claude Code pauses its sweep when it cannot read its settings.
//
// Once a second account shares the store, two more things fail: sharing
// accounts reading separate settings files, because agreement between
// separate files lasts only until one of them is edited, and an unset period,
// because the default is the vendor's to change in any release and a change
// prunes every account's history at once. A primary alone shares nothing, so
// its default is reported, never judged. The period's value stays the user's
// policy, printed on the ok line.
func checkRetention(set accounts.Set, r *report) {
	var periods, unsetNames []string
	var files []settingsGroup
	var missing []string
	shortest, longest, sharing := 0, 0, 0
	for _, a := range set.Accounts {
		if accounts.VerifyTopology(a) != nil {
			continue // not sharing the store; its topology line fails instead
		}
		sharing++
		name := display(a)
		path := filepath.Join(a.Dir(), "settings.json")
		files, missing = groupSettings(files, missing, name, path)
		days, isSet, err := cleanupPeriod(path)
		if err != nil {
			r.skip(fmt.Sprintf("retention[%s]: not tested", name),
				fmt.Sprintf("%v — Claude Code pauses its cleanup sweep until it can read the setting", err))
			continue
		}
		label := fmt.Sprintf("%s %d", name, days)
		if !isSet {
			label += " (unset)"
			unsetNames = append(unsetNames, name)
		}
		periods = append(periods, label)
		if shortest == 0 || days < shortest {
			shortest = days
		}
		longest = max(longest, days)
	}
	shared := sharing >= 2
	if shared {
		var who []string
		for _, g := range files {
			who = append(who, fmt.Sprintf("%s reads %s", strings.Join(g.names, ", "), g.path))
		}
		for _, m := range missing {
			who = append(who, m)
		}
		r.own(len(files) == 1 && len(missing) == 0,
			"settings: every account sharing the session store reads one settings.json",
			strings.Join(who, "; ")+" — each account's cleanup sweep prunes the shared store with its own file's cleanupPeriodDays, and separate files drift apart; "+
				"make every account's settings.json a symlink to one file (`accounts add --share-config` seeds it linked to the primary's)")
	}
	if len(periods) == 0 {
		return
	}
	label := fmt.Sprintf("retention: every account sharing the session store prunes it after %d days", shortest)
	if len(unsetNames) == len(periods) {
		label += " (cleanupPeriodDays unset — Claude Code's default)"
	}
	var why []string
	if shortest != longest {
		why = append(why, fmt.Sprintf("accounts disagree on cleanupPeriodDays (%s) — every account's cleanup sweep prunes the shared store, so it keeps only %d days; "+
			"set one value in every account's settings.json (`accounts add --share-config` links them to one file)", strings.Join(periods, ", "), shortest))
	}
	if shared && len(unsetNames) > 0 {
		why = append(why, fmt.Sprintf("cleanupPeriodDays unset (%s) — the shared store is then pruned on Claude Code's default (%d days as of 2.1.287), which any release may change; "+
			"set cleanupPeriodDays explicitly in the settings.json every account reads", strings.Join(unsetNames, ", "), defaultCleanupPeriodDays))
	}
	r.own(len(why) == 0, label, strings.Join(why, "; "))
}

// settingsGroup is the accounts whose settings.json is one file, by inode.
type settingsGroup struct {
	names []string
	path  string // resolved, for the report
	fi    fs.FileInfo
}

// groupSettings adds one account's settings.json to the groups of accounts
// reading the same file. Identity is by inode, never by link text, so a link
// to a link (an account linked to a primary that is itself stowed) is the
// file it ends at. An account with no readable file is listed apart.
func groupSettings(groups []settingsGroup, missing []string, name, path string) ([]settingsGroup, []string) {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, lerr := os.Lstat(path); lerr == nil {
			return groups, append(missing, name+"'s settings.json is a dangling link")
		}
		return groups, append(missing, name+" has no settings.json")
	case err != nil:
		return groups, append(missing, fmt.Sprintf("%s's settings.json cannot be resolved (%v)", name, err))
	}
	for i := range groups {
		if os.SameFile(groups[i].fi, fi) {
			groups[i].names = append(groups[i].names, name)
			return groups, missing
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	return append(groups, settingsGroup{names: []string{name}, path: resolved, fi: fi}), missing
}

// cleanupPeriod reads cleanupPeriodDays from one settings.json: absent file or
// key is the vendor default, unset.
func cleanupPeriod(path string) (days int, isSet bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaultCleanupPeriodDays, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var s struct {
		CleanupPeriodDays *int `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, false, fmt.Errorf("%s: %v", path, err)
	}
	switch {
	case s.CleanupPeriodDays == nil:
		return defaultCleanupPeriodDays, false, nil
	case *s.CleanupPeriodDays < 1:
		return 0, false, fmt.Errorf("%s: cleanupPeriodDays is %d, which Claude Code rejects (minimum 1)", path, *s.CleanupPeriodDays)
	}
	return *s.CleanupPeriodDays, true, nil
}
