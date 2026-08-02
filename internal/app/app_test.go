package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
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

// Regression test for the picker data race: the consumer applies each result
// and then — like runSelect's draw — reads every account's view, while other
// fetches are still in flight. Fetch goroutines writing views themselves
// made this fail under -race; launchFetches must keep views single-writer.
func TestLaunchFetchesSingleWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "slow") {
			time.Sleep(50 * time.Millisecond)
		}
		w.Write([]byte(`{"limits":[{"kind":"session","percent":5,"resets_at":"2026-08-02T15:00:00Z"}]}`))
	}))
	defer srv.Close()

	cfg := config.Config{UsageURL: srv.URL}
	list := []*accountData{
		{Token: "fast", NeedsFetch: true, View: render.AccountView{Status: render.StatusPending}},
		{Token: "slow", NeedsFetch: true, View: render.AccountView{Status: render.StatusPending}},
	}
	for u := range launchFetches(context.Background(), cfg, list) {
		resolve(list[u.idx], u.res)
		for _, d := range list {
			_ = d.View.Status
			_ = len(d.View.Rows)
		}
	}
	for i, d := range list {
		if d.View.Status != render.StatusRows {
			t.Errorf("account %d not resolved: status %v", i, d.View.Status)
		}
	}
}
