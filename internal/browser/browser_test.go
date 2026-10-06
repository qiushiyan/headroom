package browser

import (
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
	profiles := ParseLocalState([]byte(localState))
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

func TestParseLocalStateTolerates(t *testing.T) {
	for _, raw := range []string{``, `not json`, `{}`, `{"profile":{}}`} {
		if got := ParseLocalState([]byte(raw)); len(got) != 0 {
			t.Errorf("%q: got %v", raw, got)
		}
	}
	if got := ReadProfiles("/nonexistent/Local State"); got != nil {
		t.Errorf("missing file: got %v", got)
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
