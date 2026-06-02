package core_test

import (
	"testing"
	"time"

	. "github.com/rancher/gitsim/pkg/core"
)

// TestStaticContentRoundtrip verifies that StaticContent returns an independent
// copy of the file map on each call.
func TestStaticContentRoundtrip(t *testing.T) {
	files := map[string][]byte{
		"README.md": []byte("hello"),
		"main.go":   []byte("package main"),
	}
	cp := NewStaticContent(files)

	got, err := cp.Files(ContentContext{Repo: "r", Branch: "main", CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(files) {
		t.Fatalf("want %d files, got %d", len(files), len(got))
	}
	for k, v := range files {
		if string(got[k]) != string(v) {
			t.Errorf("file %q: want %q got %q", k, v, got[k])
		}
	}

	// mutations to the returned map must not affect future calls
	got["injected"] = []byte("evil")
	got2, _ := cp.Files(ContentContext{})
	if _, ok := got2["injected"]; ok {
		t.Error("StaticContent did not return an independent copy")
	}
}

// TestRealClockSanity checks that RealClock.Now() advances.
func TestRealClockSanity(t *testing.T) {
	var c RealClock
	t0 := c.Now()
	time.Sleep(time.Millisecond)
	t1 := c.Now()
	if !t1.After(t0) {
		t.Error("RealClock.Now() did not advance")
	}
}

// TestManualClockSet checks that ManualClock.Set and Advance work.
func TestManualClockSet(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := NewManualClock(base)
	got := mc.Now()
	if !got.Equal(base) {
		t.Fatalf("NewManualClock(%v).Now() = %v, want %v", base, got, base)
	}

	later := base.Add(10 * time.Minute)

	mc.Set(later)

	if got := mc.Now(); !got.Equal(later) {
		t.Fatalf("want %v got %v", later, got)
	}
	mc.Advance(time.Hour)
	if got := mc.Now(); !got.Equal(later.Add(time.Hour)) {
		t.Fatalf("after Advance want %v got %v", later.Add(time.Hour), got)
	}
}

// TestSentinelErrors ensures sentinel errors are distinct.
func TestSentinelErrors(t *testing.T) {
	if ErrNotFound == ErrAlreadyExists {
		t.Error("ErrNotFound and ErrAlreadyExists must be distinct")
	}
	if ErrNotFound == ErrNoCommits {
		t.Error("ErrNotFound and ErrNoCommits must be distinct")
	}
}
