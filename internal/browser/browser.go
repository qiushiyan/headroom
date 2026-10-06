// Package browser finds the Chrome profile signed in to an account and opens
// a URL in it. `headroom login` uses it to put a vendor's approval page in
// front of the browser profile that holds that account's web session, so the
// owner's part of a login is one click. Chrome's `Local State` is read, never
// written; nothing here drives the browser beyond `open`.
package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProfileEnv carries the chosen profile directory from `headroom login` to
// the headroom process the vendor runs as $BROWSER. Present (even empty)
// with a single https URL argument, it makes headroom an opener and nothing
// else; empty means the default browser.
const ProfileEnv = "HEADROOM_BROWSER_PROFILE"

// Profile is one entry of Chrome's profile cache.
type Profile struct {
	Dir      string // directory under the user data dir: "Default", "Profile 3"
	Name     string // display name the owner gave it
	UserName string // Google account signed in to Chrome, "" when none
}

// Label is how a plan line names the profile.
func (p Profile) Label() string {
	if p.Name == "" || p.Name == p.Dir {
		return p.Dir
	}
	return p.Name + " (" + p.Dir + ")"
}

// LocalStatePath is Chrome's profile registry on macOS.
func LocalStatePath(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Local State")
}

// ReadProfiles reads the registry at path. A missing or unreadable file is
// no profiles: every account then opens in the default browser.
func ReadProfiles(path string) []Profile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return ParseLocalState(data)
}

// ParseLocalState extracts .profile.info_cache, ordered by directory so a
// plan prints the same way each run.
func ParseLocalState(data []byte) []Profile {
	var doc struct {
		Profile struct {
			InfoCache map[string]struct {
				Name     string `json:"name"`
				UserName string `json:"user_name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	out := make([]Profile, 0, len(doc.Profile.InfoCache))
	for dir, p := range doc.Profile.InfoCache {
		out = append(out, Profile{Dir: dir, Name: p.Name, UserName: p.UserName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// Match finds the one profile for email: the profile signed in to that
// Google account, else the one whose display name is the email or its local
// part, among profiles not signed in to some other Google account — Chrome's
// own sign-in says whose a profile is. Either rule must single out exactly
// one profile; two candidates are no answer, because opening the wrong one
// logs the dir in as someone else.
func Match(profiles []Profile, email string) (Profile, bool) {
	email = strings.TrimSpace(email)
	if email == "" {
		return Profile{}, false
	}
	if p, ok := only(profiles, func(p Profile) bool { return strings.EqualFold(p.UserName, email) }); ok {
		return p, true
	}
	local, _, _ := strings.Cut(email, "@")
	return only(profiles, func(p Profile) bool {
		if p.UserName != "" {
			return false
		}
		return strings.EqualFold(p.Name, email) || (local != "" && strings.EqualFold(p.Name, local))
	})
}

func only(profiles []Profile, keep func(Profile) bool) (Profile, bool) {
	var found []Profile
	for _, p := range profiles {
		if keep(p) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return Profile{}, false
	}
	return found[0], true
}

// OpenArgs is the `open(1)` argument list that shows url in the profile at
// dir, or in the default browser when dir is empty. -n is what makes Chrome
// honor --profile-directory while it is already running: the new instance
// hands the URL to the running one, in that profile's window.
func OpenArgs(dir, url string) []string {
	if dir == "" {
		return []string{url}
	}
	return []string{"-na", "Google Chrome", "--args", "--profile-directory=" + dir, url}
}

// IsOpenRequest reports whether args are the one URL a vendor hands its
// $BROWSER, so the opener mode cannot swallow a real command.
func IsOpenRequest(args []string) bool {
	return len(args) == 1 && strings.HasPrefix(args[0], "https://")
}
