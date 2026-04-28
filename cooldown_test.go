package llmagent

import (
	"testing"
	"time"
)

func fixedTracker(t *testing.T, base time.Time) *CooldownTracker {
	t.Helper()
	c := NewCooldownTracker()
	c.now = func() time.Time { return base }
	return c
}

func TestTrackerSetAndQuery(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := fixedTracker(t, now)

	if c.IsCoolingDown("gemini") {
		t.Error("expected no cooldown initially")
	}
	c.Set("gemini", 2*time.Hour, "test")
	if rem := c.Remaining("gemini"); rem != 2*time.Hour {
		t.Errorf("Remaining = %v, want 2h", rem)
	}
	if c.IsCoolingDown("claude") {
		t.Error("claude should be unaffected")
	}

	c.now = func() time.Time { return now.Add(2*time.Hour + time.Second) }
	if c.IsCoolingDown("gemini") {
		t.Error("expected expiry")
	}
}

func TestTrackerClear(t *testing.T) {
	c := NewCooldownTracker()
	c.Set("gemini", time.Hour, "test")
	c.Clear("gemini")
	if c.IsCoolingDown("gemini") {
		t.Error("Clear did not remove cooldown")
	}
}

func TestTrackerSelectSomeAvailable(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := fixedTracker(t, now)
	c.Set("gemini-pro", 8*time.Hour, "")
	c.Set("gemini-flash", 4*time.Hour, "")

	got := c.Select([]string{"gemini-pro", "gemini-flash", "claude", "codex"})
	want := []string{"claude", "codex"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Select = %v, want %v", got, want)
	}
}

func TestTrackerSelectAllCoolingSortedByExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := fixedTracker(t, now)
	c.Set("a", 8*time.Hour, "")
	c.Set("b", 2*time.Hour, "") // soonest
	c.Set("c", 4*time.Hour, "")

	got := c.Select([]string{"a", "b", "c"})
	want := []string{"b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Select[%d] = %q, want %q (got=%v)", i, got[i], want[i], got)
		}
	}
}

func TestTrackerSelectExpiredAllowed(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := fixedTracker(t, now)
	c.Set("a", time.Hour, "")
	c.now = func() time.Time { return now.Add(2 * time.Hour) }

	if got := c.Select([]string{"a", "b"}); len(got) != 2 {
		t.Errorf("Select = %v, want both", got)
	}
}

func TestTrackerOverwriteWithLonger(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := fixedTracker(t, now)
	c.Set("a", time.Hour, "")
	c.Set("a", 3*time.Hour, "")
	if c.Remaining("a") != 3*time.Hour {
		t.Errorf("Remaining = %v, want 3h", c.Remaining("a"))
	}
}

func TestStatusesAndFormat(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := fixedTracker(t, now)
	c.Set("a", time.Hour, "quota: resets in 1h")
	statuses := c.Statuses([]string{"a", "b"})
	if len(statuses) != 1 || statuses[0].Provider != "a" {
		t.Fatalf("Statuses = %+v", statuses)
	}
	if got := FormatStatuses(statuses); got != "a=1h0m0s (quota: resets in 1h)" {
		t.Errorf("FormatStatuses = %q", got)
	}
}
