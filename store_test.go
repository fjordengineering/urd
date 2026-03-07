package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "urd.json")
	return &Store{FilePath: path}
}

func TestLoadStoreNonExistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(s.Streams) != 0 {
		t.Fatalf("expected empty streams, got %d", len(s.Streams))
	}
}

func TestSaveAndLoad(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("Test", 0)
	if err := s.Save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := LoadStore(s.FilePath)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(loaded.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(loaded.Streams))
	}
	if loaded.Streams[0].Name != "Test" {
		t.Fatalf("expected name 'Test', got %q", loaded.Streams[0].Name)
	}
}

func TestAddStreamPositions(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.AddStream("B", 0)
	s.AddStream("C", 1)

	if len(s.Streams) != 3 {
		t.Fatalf("expected 3 streams, got %d", len(s.Streams))
	}
	if s.Streams[0].Name != "B" {
		t.Fatalf("expected 'B' at 0, got %q", s.Streams[0].Name)
	}
	if s.Streams[1].Name != "C" {
		t.Fatalf("expected 'C' at 1, got %q", s.Streams[1].Name)
	}
	if s.Streams[2].Name != "A" {
		t.Fatalf("expected 'A' at 2, got %q", s.Streams[2].Name)
	}
}

func TestAddStreamEmptyList(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("First", 0)
	if len(s.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(s.Streams))
	}
	if s.Streams[0].Name != "First" {
		t.Fatalf("expected 'First', got %q", s.Streams[0].Name)
	}
}

func TestAddStreamOutOfBounds(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 100)
	if len(s.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(s.Streams))
	}
	s.AddStream("B", -5)
	if s.Streams[0].Name != "B" {
		t.Fatalf("expected 'B' at 0, got %q", s.Streams[0].Name)
	}
}

func TestDeleteStream(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.AddStream("B", 1)
	id := s.Streams[0].ID
	s.DeleteStream(id)

	if len(s.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(s.Streams))
	}
	if s.Streams[0].Name != "B" {
		t.Fatalf("expected 'B', got %q", s.Streams[0].Name)
	}
}

func TestDeleteNonExistent(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.DeleteStream("bogus")
	if len(s.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(s.Streams))
	}
}

func TestToggleStreamActivateDeactivate(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	id := s.Streams[0].ID

	s.ToggleStream(id)
	if !s.Streams[0].Active {
		t.Fatal("expected stream to be active")
	}
	if s.Streams[0].StartedAt == nil {
		t.Fatal("expected StartedAt to be set")
	}
	if len(s.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(s.Sessions))
	}
	if s.Sessions[0].End != nil {
		t.Fatal("expected open session")
	}

	s.ToggleStream(id)
	if s.Streams[0].Active {
		t.Fatal("expected stream to be inactive")
	}
	if s.Sessions[0].End == nil {
		t.Fatal("expected session to be closed")
	}
}

func TestMultipleActiveStreams(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.AddStream("B", 1)

	s.ToggleStream(s.Streams[0].ID)
	s.ToggleStream(s.Streams[1].ID)

	if !s.Streams[0].Active || !s.Streams[1].Active {
		t.Fatal("expected both streams active")
	}
	// Only one session should be open (second activate doesn't create new session)
	if len(s.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(s.Sessions))
	}

	// Deactivate one — session stays open
	s.ToggleStream(s.Streams[0].ID)
	if s.Sessions[0].End != nil {
		t.Fatal("session should remain open with one active stream")
	}

	// Deactivate last — session closes
	s.ToggleStream(s.Streams[1].ID)
	if s.Sessions[0].End == nil {
		t.Fatal("session should be closed")
	}
}

func TestStopAll(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.AddStream("B", 1)
	s.ToggleStream(s.Streams[0].ID)
	s.ToggleStream(s.Streams[1].ID)

	s.StopAll()

	if s.HasActive() {
		t.Fatal("expected no active streams")
	}
	if s.Sessions[0].End == nil {
		t.Fatal("expected session closed")
	}
}

func TestStopAllRemembersLastActive(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.AddStream("B", 1)
	s.AddStream("C", 2)
	idA := s.Streams[0].ID
	idB := s.Streams[1].ID

	s.ToggleStream(idA)
	s.ToggleStream(idB)
	s.StopAll()

	if len(s.LastActive) != 2 {
		t.Fatalf("expected 2 last active, got %d", len(s.LastActive))
	}
}

func TestContinueAll(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.AddStream("B", 1)
	s.AddStream("C", 2)
	idA := s.Streams[0].ID
	idB := s.Streams[1].ID

	s.ToggleStream(idA)
	s.ToggleStream(idB)
	s.StopAll()
	s.ContinueAll()

	if !s.Streams[0].Active || !s.Streams[1].Active {
		t.Fatal("expected A and B to be active again")
	}
	if s.Streams[2].Active {
		t.Fatal("expected C to remain inactive")
	}
	if s.LastActive != nil {
		t.Fatal("expected LastActive to be cleared")
	}
	// Should have a new open session
	last := s.Sessions[len(s.Sessions)-1]
	if last.End != nil {
		t.Fatal("expected new open session")
	}
}

func TestContinueAllNoOp(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	s.ContinueAll() // no last active
	if s.HasActive() {
		t.Fatal("expected no active streams")
	}
}

func TestHasActive(t *testing.T) {
	s := newTestStore(t)
	if s.HasActive() {
		t.Fatal("expected false for empty store")
	}
	s.AddStream("A", 0)
	if s.HasActive() {
		t.Fatal("expected false with inactive stream")
	}
	s.ToggleStream(s.Streams[0].ID)
	if !s.HasActive() {
		t.Fatal("expected true with active stream")
	}
}

func TestSortStreams(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("First", 0)
	s.AddStream("Second", 1)
	s.AddStream("Third", 2)

	now := time.Now()
	s.Streams[2].Active = true
	s.Streams[2].StartedAt = &now

	s.SortStreams()

	// Active stream should be first.
	if s.Streams[0].Name != "Third" {
		t.Fatalf("expected active stream first, got %q", s.Streams[0].Name)
	}
	// Inactive streams preserve creation order.
	if s.Streams[1].Name != "First" {
		t.Fatalf("expected 'First' second, got %q", s.Streams[1].Name)
	}
	if s.Streams[2].Name != "Second" {
		t.Fatalf("expected 'Second' third, got %q", s.Streams[2].Name)
	}
}

func TestTotalWallClock(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	end := now.Add(time.Hour)
	s.Sessions = []Session{
		{Start: now, End: &end},
	}
	wc := s.TotalWallClock()
	if wc != time.Hour {
		t.Fatalf("expected 1h, got %s", wc)
	}
}

func TestSaveAtomicWrite(t *testing.T) {
	s := newTestStore(t)
	s.AddStream("A", 0)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Tmp file should not exist after save
	if _, err := os.Stat(s.FilePath + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("expected tmp file to be removed")
	}
	// Actual file should exist
	if _, err := os.Stat(s.FilePath); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0h 00m 00s"},
		{30 * time.Second, "0h 00m 30s"},
		{5 * time.Minute, "0h 05m 00s"},
		{time.Hour + 2*time.Minute + 15*time.Second, "1h 02m 15s"},
		{25 * time.Hour, "25h 00m 00s"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestDeleteSession(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	end1 := now.Add(time.Hour)
	end2 := now.Add(2 * time.Hour)
	s.Sessions = []Session{
		{Start: now, End: &end1},
		{Start: now.Add(time.Hour), End: &end2},
	}
	s.DeleteSession(0)
	if len(s.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(s.Sessions))
	}
	if s.Sessions[0].Start != now.Add(time.Hour) {
		t.Fatal("expected second session to remain")
	}
}

func TestDeleteSessionOutOfBounds(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	end := now.Add(time.Hour)
	s.Sessions = []Session{{Start: now, End: &end}}
	s.DeleteSession(5)  // out of bounds
	s.DeleteSession(-1) // negative
	if len(s.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(s.Sessions))
	}
}

func TestUpdateSession(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	end := now.Add(2 * time.Hour)
	s.Sessions = []Session{{Start: now, End: &end}}

	newEnd := now.Add(time.Hour)
	err := s.UpdateSession(0, now, &newEnd)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if s.Sessions[0].End.Sub(s.Sessions[0].Start) != time.Hour {
		t.Fatal("expected session to be 1 hour")
	}
}

func TestSortSessionsDesc(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	e1 := now.Add(time.Hour)
	e2 := now.Add(3 * time.Hour)
	s.Sessions = []Session{
		{Start: now, End: &e1},
		{Start: now.Add(2 * time.Hour), End: &e2},
	}
	s.SortSessionsDesc()
	if !s.Sessions[0].Start.After(s.Sessions[1].Start) {
		t.Fatal("expected newest session first")
	}
}

func TestAddPastTime(t *testing.T) {
	s := newTestStore(t)
	start := time.Now().Add(-time.Hour)
	end := time.Now()
	s.AddPastTime(start, end)
	if len(s.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(s.Sessions))
	}
	if s.Sessions[0].End == nil {
		t.Fatal("expected closed session")
	}
}
