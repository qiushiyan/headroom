package browser

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The shape of the owner's Local State on both machines, 2026-10-06.
const localState = `{"profile":{"info_cache":{
  "Default":{"name":"Qiushi","user_name":"qiushi.yann@gmail.com"},
  "Profile 2":{"name":"cliushi","user_name":""},
  "Profile 3":{"name":"qiushi","user_name":""},
  "Profile 4":{"name":"LEAR","user_name":"lear.lab.vu@gmail.com"}
}},"other":1}`

func TestMatch(t *testing.T) {
	profiles, err := ParseLocalState([]byte(localState))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		email, want string
		ok          bool
	}{
		{"qiushi.yann@gmail.com", "Default", true}, // signed-in Google account
		{"QIUSHI.YANN@gmail.com", "Default", true},
		{"cliushi@planlab.ai", "Profile 2", true}, // display name is the local part
		{"qiushi@planlab.ai", "Profile 3", true},  // "Qiushi" is Default's name, but Default is signed in to another account
		{"yan@planlab.ai", "", false},             // no profile
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := Match(profiles, c.email)
		if ok != c.ok || got.Dir != c.want {
			t.Errorf("Match(%q) = %q,%v; want %q,%v", c.email, got.Dir, ok, c.want, c.ok)
		}
	}
}

// The signed-in account outranks a name, and two profiles answering to one
// name are no answer: the wrong profile would log the dir in as someone else.
func TestMatchPrefersSignInAndRefusesAmbiguity(t *testing.T) {
	profiles := []Profile{
		{Dir: "Profile 1", Name: "yan"},
		{Dir: "Profile 2", Name: "work", UserName: "yan@x.com"},
	}
	if got, ok := Match(profiles, "yan@x.com"); !ok || got.Dir != "Profile 2" {
		t.Errorf("sign-in match: got %q,%v", got.Dir, ok)
	}
	twins := []Profile{{Dir: "Profile 1", Name: "yan"}, {Dir: "Profile 2", Name: "YAN"}}
	if got, ok := Match(twins, "yan@x.com"); ok {
		t.Errorf("ambiguous names matched %q", got.Dir)
	}
}

// A registry with no profiles is no profiles; one that is not JSON, or no
// file at all, is no profiles and says why, so a plan can tell "nothing
// matched" from "nothing was read".
func TestParseLocalStateTolerates(t *testing.T) {
	for _, raw := range []string{`{}`, `{"profile":{}}`} {
		if got, err := ParseLocalState([]byte(raw)); len(got) != 0 || err != nil {
			t.Errorf("%q: got %v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{``, `not json`} {
		if got, err := ParseLocalState([]byte(raw)); len(got) != 0 || err == nil {
			t.Errorf("%q: got %v, %v; want an error", raw, got, err)
		}
	}
	if got, err := ReadProfiles("/nonexistent/Local State"); got != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: got %v, %v", got, err)
	}
	unreadable := filepath.Join(t.TempDir(), "Local State")
	if err := os.WriteFile(unreadable, []byte(localState), 0o000); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadProfiles(unreadable); got != nil || err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unreadable file: got %v, %v", got, err)
	}
}

func TestOpenArgs(t *testing.T) {
	if got := OpenArgs("", "https://x"); !reflect.DeepEqual(got, []string{"https://x"}) {
		t.Errorf("default browser: %v", got)
	}
	want := []string{"-na", "Google Chrome", "--args", "--profile-directory=Profile 3", "https://x"}
	if got := OpenArgs("Profile 3", "https://x"); !reflect.DeepEqual(got, want) {
		t.Errorf("profile: %v", got)
	}
}

func TestIsOpenRequest(t *testing.T) {
	for args, want := range map[string]bool{"https://claude.com/x": true, "http://x": false, "accounts": false} {
		if got := IsOpenRequest([]string{args}); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
	if IsOpenRequest([]string{"https://x", "more"}) || IsOpenRequest(nil) {
		t.Error("only a single URL is an open request")
	}
}
