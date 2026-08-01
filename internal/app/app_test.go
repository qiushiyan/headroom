package app

import (
	"errors"
	"net/http"
	"testing"

	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/usage"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name string
		res  usage.Result
		want render.Status
	}{
		{"transport error", usage.Result{Err: errors.New("timeout")}, render.StatusFetchFailed},
		{"auth rejected", usage.Result{StatusCode: http.StatusUnauthorized}, render.StatusHTTPError},
		{"unparseable body", usage.Result{StatusCode: 200, Body: []byte(`nope`)}, render.StatusUnparseable},
		{"no limits", usage.Result{StatusCode: 200, Body: []byte(`{}`)}, render.StatusNoLimits},
		{"rows", usage.Result{StatusCode: 200,
			Body: []byte(`{"limits":[{"kind":"session","percent":5,"resets_at":"2026-08-02T15:00:00Z"}]}`)}, render.StatusRows},
	}
	for _, c := range cases {
		d := &accountData{}
		resolve(d, c.res)
		if d.View.Status != c.want {
			t.Errorf("%s: status = %v, want %v", c.name, d.View.Status, c.want)
		}
	}

	d := &accountData{}
	resolve(d, usage.Result{StatusCode: http.StatusUnauthorized})
	if d.View.HTTPCode != http.StatusUnauthorized {
		t.Errorf("HTTP code not carried: %+v", d.View)
	}
	d = &accountData{}
	resolve(d, usage.Result{StatusCode: 200,
		Body: []byte(`{"limits":[{"kind":"session","percent":5},{"group":"weekly","percent":9}]}`)})
	if len(d.View.Rows) != 2 {
		t.Errorf("rows not carried: %+v", d.View)
	}
}
