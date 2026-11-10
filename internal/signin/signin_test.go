package signin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func begun(t *testing.T) (*Signin, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "k.json")
	s, err := Begin(path, Record{Account: "a@x.com", Email: "a@x.com", URL: "https://x/authorize?state=st", SigninState: "st"}, time.UnixMilli(1000), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func onDisk(t *testing.T, path string) Record {
	t.Helper()
	rec, found, err := Read(path)
	if err != nil || !found {
		t.Fatalf("read: found %v, %v", found, err)
	}
	return rec
}

// A sign-in's steps are taken by writing them: begun waiting, with its
// deadline; exchanging once, and only from waiting; ended with the URL gone
// and the time it ended.
func TestASigninMovesByItsRecord(t *testing.T) {
	s, path := begun(t)
	if rec := onDisk(t, path); rec.State != Awaiting || rec.URL == "" || rec.StartedMS != 1000 || rec.ExpiresMS != 1000+10*60*1000 || rec.FinishedMS != 0 {
		t.Fatalf("begun: %+v", rec)
	}
	if err := s.BeginExchange(); err != nil {
		t.Fatal(err)
	}
	if rec := onDisk(t, path); rec.State != Exchanging || s.State() != Exchanging {
		t.Fatalf("exchanging: %+v", rec)
	}
	if err := s.BeginExchange(); err != ErrNotAwaiting {
		t.Errorf("a second code was authorized: %v", err)
	}
	rec, err := s.End(EndRenewed(777), time.UnixMilli(5000))
	if err != nil {
		t.Fatal(err)
	}
	if disk := onDisk(t, path); disk != rec || rec.State != Renewed || rec.URL != "" || rec.FinishedMS != 5000 || rec.LoginEndsMS != 777 || rec.Reason != "" {
		t.Errorf("ended: %+v, on disk %+v", rec, disk)
	}
	if err := s.BeginExchange(); err != ErrNotAwaiting {
		t.Errorf("a code was authorized for a sign-in that is over: %v", err)
	}

	failed, _ := begun(t)
	if rec, _ := failed.End(EndFailed("expired", "no code came"), time.UnixMilli(9)); rec.State != Failed || rec.Reason != "expired" || rec.Problem != "no code came" || rec.URL != "" || rec.LoginEndsMS != 0 {
		t.Errorf("failed: %+v", rec)
	}
}

// The two steps fail differently when the record cannot be written, and the
// difference is the point. A code is authorized only by `exchanging` on disk,
// so that step is not taken. An ending already happened, so it is returned as
// what happened, and the record keeps the last step it could.
func TestAStepThatCannotBeRecorded(t *testing.T) {
	s, path := begun(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := s.BeginExchange(); err == nil {
		t.Fatal("a code was authorized that the record does not show")
	}
	if s.State() != Awaiting || onDisk(t, path).State != Awaiting {
		t.Errorf("the sign-in left waiting: %s, on disk %s", s.State(), onDisk(t, path).State)
	}

	rec, err := s.End(EndFailed("cancelled", "stopped"), time.UnixMilli(5000))
	if err == nil {
		t.Fatal("an ending that was not saved reported no error")
	}
	if rec.State != Failed || rec.Reason != "cancelled" || rec.FinishedMS != 5000 {
		t.Errorf("the ending returned is not what happened: %+v", rec)
	}
	if disk := onDisk(t, path); disk.State != Awaiting || s.State() != Awaiting {
		t.Errorf("the record must keep the last step it could: on disk %s, held %s", disk.State, s.State())
	}
}

// A record whose holder is gone: live ones ended with it, before any code or
// with one's outcome unseen, and nobody knows when. A record already over is
// left as it is.
func TestOrphaned(t *testing.T) {
	for _, c := range []struct {
		in                 Record
		state, reason, url string
		finishedMS, endsMS int64
	}{
		{Record{State: Awaiting, URL: "u", Problem: "old", StartedMS: 5}, Failed, ReasonHolderGone, "", 0, 0},
		{Record{State: Exchanging, URL: "u", StartedMS: 5}, Unknown, ReasonOutcomeUnknown, "", 0, 0},
		{Record{State: Renewed, FinishedMS: 9, LoginEndsMS: 99}, Renewed, "", "", 9, 99},
		{Record{State: Failed, Reason: "expired", FinishedMS: 9}, Failed, "expired", "", 9, 0},
		{Record{State: None}, None, "", "", 0, 0},
	} {
		got := c.in.Orphaned()
		if got.State != c.state || got.Reason != c.reason || got.URL != c.url || got.FinishedMS != c.finishedMS || got.LoginEndsMS != c.endsMS || got.StartedMS != c.in.StartedMS {
			t.Errorf("%+v orphaned = %+v", c.in, got)
		}
		if Live(got.State) || (c.in.State != None && !Over(got.State)) {
			t.Errorf("%s orphaned is still %s", c.in.State, got.State)
		}
	}
}
