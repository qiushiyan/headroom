package auth

import "testing"

func TestParse(t *testing.T) {
	st, ok := Parse([]byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",
	  "email":"a@x.com","orgId":"o","orgName":"O","subscriptionType":"max"}`))
	if !ok || !st.LoggedIn || st.Email != "a@x.com" || st.SubscriptionType != "max" || !st.Parsed {
		t.Errorf("logged-in status: %+v ok=%v", st, ok)
	}

	st, ok = Parse([]byte(`{"loggedIn":false}`))
	if !ok || st.LoggedIn || !st.Parsed {
		t.Errorf("logged-out status: %+v ok=%v", st, ok)
	}
}

// Every failure to get an answer must read as "unknown", never as "logged
// out". An absent Claude Code binary, a changed output shape, or a crash must
// not make four healthy accounts look dead — the caller falls back to
// credential evidence only when Parsed is false.
func TestUnanswerableIsUnknownNotLoggedOut(t *testing.T) {
	for _, raw := range []string{
		``,
		`not json`,
		`{}`,                  // shape drifted: no loggedIn field
		`{"email":"a@x.com"}`, // partial
		`{"loggedIn":"yes"}`,  // wrong type
		`{"loggedIn":null}`,   // explicit null
	} {
		st, ok := Parse([]byte(raw))
		if ok || st.Parsed || st.LoggedIn {
			t.Errorf("%q: got %+v ok=%v, want unparsed and not logged in", raw, st, ok)
		}
	}
}
