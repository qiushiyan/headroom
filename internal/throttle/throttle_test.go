package throttle

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func TestEligibilityLifecycle(t *testing.T) {
	root := t.TempDir()
	s := Load(root)
	// Aligned to the store's millisecond resolution so exact comparisons mean
	// what they say.
	now := time.UnixMilli(time.Now().UnixMilli())

	// An account never fetched is free to go.
	if !s.Eligible("a", now) {
		t.Error("a never-fetched account should be eligible")
	}

	s.NoteAttempt("a", now)
	if s.Eligible("a", now) {
		t.Error("an account just fetched should be holding quiet")
	}
	if !s.Eligible("a", now.Add(Spacing+time.Second)) {
		t.Error("quiet period never ended")
	}

	// A refusal buys a longer silence than an ordinary attempt, and each
	// consecutive one buys more — because a refused request may itself count
	// against the budget, probing to find recovery can prevent it.
	s.NoteRefused("a", now)
	first := s.NextEligible("a")
	if !first.After(now.Add(Spacing)) {
		t.Errorf("refusal cooldown not longer than ordinary spacing: %v", first.Sub(now))
	}
	s.NoteRefused("a", now)
	if !s.NextEligible("a").After(first) {
		t.Error("consecutive refusals should back off further")
	}

	// Only a completed request proves the budget recovered.
	s.NoteSuccess("a", now)
	if got := s.NextEligible("a"); !got.Equal(now.Add(Spacing)) {
		t.Errorf("success should reset to ordinary spacing, got %v", got.Sub(now))
	}
	s.NoteRefused("a", now)
	if !s.NextEligible("a").Equal(now.Add(CooldownBase)) {
		t.Error("strike count not cleared by success")
	}
}

// One account's refusal says nothing about another's budget — the endpoint
// limits per account. A fleet-wide backoff punished healthy accounts.
func TestBackoffIsPerAccount(t *testing.T) {
	s := Load(t.TempDir())
	now := time.Now()
	s.NoteRefused("hot", now)
	if s.Eligible("hot", now) {
		t.Error("refused account should be cooling down")
	}
	if !s.Eligible("cold", now) {
		t.Error("an unrelated account was penalised for another's refusal")
	}
}

// The store's whole purpose is coordinating separate processes, so state has
// to survive one exiting.
func TestStorePersistsAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	s := Load(root)
	s.NoteRefused("a", now)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	reopened := Load(root)
	if reopened.Eligible("a", now) {
		t.Error("cooldown did not survive a reload")
	}
	if !reopened.NextEligible("a").Equal(s.NextEligible("a")) {
		t.Error("next-eligible time not round-tripped")
	}
}

// Throttling is advisory: losing or corrupting the file must degrade to
// "everything is eligible", never stop the tool working.
func TestCorruptStoreIsEmptyNotFatal(t *testing.T) {
	root := t.TempDir()
	if err := writeFile(filepath.Join(root, ".throttle"), "{not json"); err != nil {
		t.Fatal(err)
	}
	if !Load(root).Eligible("a", time.Now()) {
		t.Error("a corrupt store should not block requests")
	}
}

// Save is a no-op when nothing changed, so a read-only surface never touches
// the disk.
func TestSaveIsNoOpWhenUnchanged(t *testing.T) {
	root := t.TempDir()
	s := Load(root)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(filepath.Join(root, ".throttle")); err == nil {
		t.Error("an unmodified store should not have written a file")
	}
}
