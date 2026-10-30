package accounts

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

func TestReservedFileIsReadStrictly(t *testing.T) {
	cfg := config.Scope{AccountsRoot: t.TempDir()}
	if held, err := ReadReservations(cfg); err != nil || held != nil {
		t.Fatalf("absent = %v, %v", held, err)
	}
	for name, body := range map[string]string{
		"empty":        "",
		"no tab":       "a@x.com /dev/itell\n",
		"no account":   "\t/dev/itell\n",
		"relative dir": "a@x.com\tdev/itell\n",
		"blank line":   "a@x.com\t/dev/itell\n\n",
	} {
		if err := os.WriteFile(cfg.ReservedFile(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if held, err := ReadReservations(cfg); err == nil || !strings.Contains(err.Error(), ".reserved") {
			t.Errorf("%s: %v, %v", name, held, err)
		}
	}
}

func TestSetReservationRoundTripsAndGoesWithItsLastRow(t *testing.T) {
	cfg := config.Scope{AccountsRoot: t.TempDir()}
	if err := SetReservation(cfg, "b@x.com", nil); err != nil {
		t.Fatalf("dropping what is not there: %v", err)
	}
	for name, dirs := range map[string][]string{"a@x.com": {"/dev/itell", "/dev/itell-cms"}, "b@x.com": {"/dev/other"}} {
		if err := SetReservation(cfg, name, dirs); err != nil {
			t.Fatal(err)
		}
	}
	held, err := ReadReservations(cfg)
	want := Reservations{"a@x.com": {"/dev/itell", "/dev/itell-cms"}, "b@x.com": {"/dev/other"}}
	if err != nil || !reflect.DeepEqual(held, want) {
		t.Fatalf("read back %v, %v", held, err)
	}
	if err := SetReservation(cfg, "a@x.com", []string{"/dev/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if held, _ := ReadReservations(cfg); !reflect.DeepEqual(held["a@x.com"], []string{"/dev/elsewhere"}) {
		t.Errorf("a new reservation replaces the old: %v", held)
	}
	for _, name := range []string{"a@x.com", "b@x.com"} {
		if err := SetReservation(cfg, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(cfg.ReservedFile()); !os.IsNotExist(err) {
		t.Errorf("the last row's file remains: %v", err)
	}
}

func TestAdmitsComparesPathComponents(t *testing.T) {
	held := Reservations{"a": {"/dev/itell", "/"}, "b": {"/dev/itell"}}
	for _, c := range []struct {
		name, project string
		want          bool
	}{
		{"b", "/dev/itell", true},
		{"b", "/dev/itell/apps/platform", true},
		{"b", "/dev/itell-cms", false},
		{"b", "/dev", false},
		{"b", "", false},
		{"c", "/dev/itell", false},
		{"a", "/anywhere", true},
	} {
		if got := held.Admits(c.name, c.project); got != c.want {
			t.Errorf("Admits(%s, %q) = %v", c.name, c.project, got)
		}
	}
	if !held.Held("a") || held.Held("c") {
		t.Error("Held")
	}
}
