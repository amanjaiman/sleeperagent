package statefile

import (
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	t.Setenv("SLEEPERAGENT_STATE_DIR", t.TempDir())

	in := Record{
		Name:       "feature-x",
		Agent:      "claude",
		Session:    "feature-x",
		State:      "WAITING",
		ResetTime:  time.Date(2026, 6, 26, 14, 0, 0, 0, time.UTC),
		WaitUntil:  time.Date(2026, 6, 26, 14, 1, 0, 0, time.UTC),
		PromptText: "continue",
		PID:        4321,
	}
	if err := Write(in); err != nil {
		t.Fatal(err)
	}
	got, err := Read("feature-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != in.Name || got.State != in.State || got.PID != in.PID {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.WaitUntil.Equal(in.WaitUntil) {
		t.Fatalf("wait_until = %v, want %v", got.WaitUntil, in.WaitUntil)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be set on write")
	}
}

func TestListAndRemove(t *testing.T) {
	t.Setenv("SLEEPERAGENT_STATE_DIR", t.TempDir())
	for _, n := range []string{"b", "a"} {
		if err := Write(Record{Name: n, Agent: "claude", State: "RUNNING"}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Name != "a" || recs[1].Name != "b" {
		t.Fatalf("List = %+v, want sorted [a b]", recs)
	}
	Remove("a")
	recs, _ = List()
	if len(recs) != 1 || recs[0].Name != "b" {
		t.Fatalf("after Remove, List = %+v", recs)
	}
}

func TestControlRoundTrip(t *testing.T) {
	t.Setenv("SLEEPERAGENT_STATE_DIR", t.TempDir())

	if _, ok, _ := TakeControl("x"); ok {
		t.Fatal("expected no pending control command")
	}
	if err := WriteControl("x", "detach"); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err := TakeControl("x")
	if err != nil || !ok || cmd != "detach" {
		t.Fatalf("TakeControl = (%q,%v,%v), want (detach,true,nil)", cmd, ok, err)
	}
	// Second take should be empty: the command is one-shot.
	if _, ok, _ := TakeControl("x"); ok {
		t.Fatal("control command should be consumed exactly once")
	}
}

func TestSafeNameRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", "..", "a/b", `a\b`, "a:b"} {
		if _, err := Path(bad); err == nil {
			t.Errorf("Path(%q) should have failed", bad)
		}
	}
}
