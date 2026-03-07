package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Stream represents a named time-tracking category. Streams are toggleable
// labels — actual time is tracked via Sessions (wall-clock periods). A stream
// only records whether it's currently active and when the current activation
// started, which is used to manage session boundaries.
type Stream struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Active    bool       `json:"active"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Session tracks a continuous wall-clock period during which at least one
// stream was active. Sessions exist separately from streams so we can show
// total wall-clock time even when the user switches between streams (which
// would otherwise create gaps). End is a pointer so that nil represents an
// ongoing session — this lets us detect unclean shutdowns (crash/force-quit)
// on the next load, since the session will still be open.
type Session struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
}

// Store is the root data structure persisted to urd.json. It owns all streams
// and sessions. FilePath is tagged `json:"-"` so it stays out of the JSON file
// — it's runtime-only state injected by LoadStore.
// LastActive records which streams were running before StopAll, enabling
// ContinueAll to resume exactly the same set. It's cleared after use.
type Store struct {
	Streams    []Stream  `json:"streams"`
	Sessions   []Session `json:"sessions"`
	LastActive []string  `json:"last_active,omitempty"`
	FilePath   string    `json:"-"`
}

// newID generates a short random hex string for stream identification.
// 3 bytes = 6 hex chars, giving 16M possible IDs. This is plenty for a
// personal time tracker (collisions are negligible) while keeping the JSON
// human-readable. We use crypto/rand over math/rand to avoid seeding concerns.
func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// LoadStore reads the JSON file at path, or returns an empty Store if the file
// doesn't exist yet (first run). A missing file is not an error because we
// want a zero-config first launch — the file is created on the first Save().
func LoadStore(path string) (*Store, error) {
	s := &Store{FilePath: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	// Safety fallback: a stream can end up Active with no StartedAt if the
	// JSON was hand-edited or if a bug wrote a partial state. Rather than
	// refusing to load, we recover by setting StartedAt to now — the stream
	// will just start counting from this moment.
	now := time.Now()
	for i := range s.Streams {
		if s.Streams[i].Active && s.Streams[i].StartedAt == nil {
			s.Streams[i].StartedAt = &now
		}
	}
	return s, nil
}

// Save writes the store to disk using an atomic write-to-temp-then-rename
// pattern. This prevents data loss if the process is killed mid-write: we
// either have the old complete file or the new complete file, never a
// half-written one. MarshalIndent is used over Marshal so the JSON file
// remains human-readable for manual inspection and debugging.
func (s *Store) Save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.FilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.FilePath)
}

// AddStream inserts a new stream at position `at` in the slice. The position
// parameter enables the o/O keybindings (add below/above cursor). Clamping
// ensures out-of-range positions don't panic — they just append to the end.
func (s *Store) AddStream(name string, at int) {
	st := Stream{
		ID:        newID(),
		Name:      name,
		CreatedAt: time.Now(),
	}
	if at < 0 {
		at = 0
	}
	if at >= len(s.Streams) {
		s.Streams = append(s.Streams, st)
		return
	}
	// Splice insert: grow the slice by one, shift elements right, then place
	// the new stream at the desired index.
	s.Streams = append(s.Streams[:at+1], s.Streams[at:]...)
	s.Streams[at] = st
}

func (s *Store) DeleteStream(id string) {
	for i, st := range s.Streams {
		if st.ID == id {
			s.Streams = append(s.Streams[:i], s.Streams[i+1:]...)
			return
		}
	}
}

// ToggleStream activates or deactivates a single stream by ID, using the
// current time. See toggleStreamAt for the full documentation.
func (s *Store) ToggleStream(id string) {
	s.toggleStreamAt(id, time.Now())
}

// ToggleStreamAt activates or deactivates a single stream by ID, using the
// provided startAt time instead of time.Now(). This lets the user backdate
// a stream activation (e.g. "I actually started 5 minutes ago"). When
// deactivating, startAt is ignored — we always use now.
func (s *Store) ToggleStreamAt(id string, startAt time.Time) {
	s.toggleStreamAt(id, startAt)
}

// toggleStreamAt is the shared implementation for ToggleStream and
// ToggleStreamAt. Session management is edge-triggered: we only open/close
// a wall-clock session when the count of active streams crosses zero. This
// means switching between streams (deactivate A, activate B) doesn't create
// a gap in the wall-clock session — only going from "nothing active" to
// "something active" (or vice versa) triggers a session boundary.
func (s *Store) toggleStreamAt(id string, startAt time.Time) {
	hadActive := s.HasActive()
	for i := range s.Streams {
		if s.Streams[i].ID == id {
			if s.Streams[i].Active {
				s.Streams[i].Active = false
				s.Streams[i].StartedAt = nil
			} else {
				t := startAt
				s.Streams[i].Active = true
				s.Streams[i].StartedAt = &t
			}
			break
		}
	}
	hasActive := s.HasActive()
	if !hadActive && hasActive {
		s.Sessions = append(s.Sessions, Session{Start: startAt})
	} else if hadActive && !hasActive {
		s.closeCurrentSession()
	}
}

// StopAll pauses every active stream and records their IDs in LastActive.
// This enables a stop/continue workflow: the user can pause everything
// (e.g. for a meeting) and later resume the exact same set with ContinueAll.
// LastActive is reset each time so it always reflects the most recent stop.
func (s *Store) StopAll() {
	hadActive := s.HasActive()
	s.LastActive = nil
	for i := range s.Streams {
		if s.Streams[i].Active {
			s.LastActive = append(s.LastActive, s.Streams[i].ID)
			s.Streams[i].Active = false
			s.Streams[i].StartedAt = nil
		}
	}
	if hadActive {
		s.closeCurrentSession()
	}
}

// ContinueAll resumes the streams that were running before the last StopAll.
// A single `now` timestamp is captured and shared across all resumed streams
// so they start from the exact same moment, keeping time accounting consistent.
// LastActive is cleared after use — continue is a one-shot operation.
func (s *Store) ContinueAll() {
	if len(s.LastActive) == 0 {
		return
	}
	ids := make(map[string]bool, len(s.LastActive))
	for _, id := range s.LastActive {
		ids[id] = true
	}
	hadActive := s.HasActive()
	now := time.Now()
	for i := range s.Streams {
		if ids[s.Streams[i].ID] && !s.Streams[i].Active {
			s.Streams[i].Active = true
			s.Streams[i].StartedAt = &now
		}
	}
	if !hadActive && s.HasActive() {
		s.Sessions = append(s.Sessions, Session{Start: now})
	}
	s.LastActive = nil
}

// closeCurrentSession finds the most recent open session and sets its End
// to now. We search backwards because the open session is always the last
// one — earlier sessions are already closed. The reverse scan is a defensive
// choice in case of data corruption.
func (s *Store) closeCurrentSession() {
	now := time.Now()
	for i := len(s.Sessions) - 1; i >= 0; i-- {
		if s.Sessions[i].End == nil {
			s.Sessions[i].End = &now
			return
		}
	}
}

// TotalWallClock returns the total non-overlapping wall-clock time spent tracking.
func (s *Store) TotalWallClock() time.Duration {
	var total time.Duration
	for _, sess := range s.Sessions {
		end := time.Now()
		if sess.End != nil {
			end = *sess.End
		}
		total += end.Sub(sess.Start)
	}
	return total.Truncate(time.Second)
}

// AddPastTime creates a closed session for a completed time block without
// activating any stream. This is for recording work that happened entirely
// in the past (e.g. a meeting from 10:00–10:45 that the user forgot to track).
func (s *Store) AddPastTime(start, end time.Time) {
	s.Sessions = append(s.Sessions, Session{Start: start, End: &end})
}

// DeleteSession removes the session at the given index by splice-removing it
// from the Sessions slice. This is index-based (not ID-based like DeleteStream)
// because sessions don't have unique identifiers — they're identified by
// position in the sorted list shown to the user.
func (s *Store) DeleteSession(index int) {
	if index < 0 || index >= len(s.Sessions) {
		return
	}
	s.Sessions = append(s.Sessions[:index], s.Sessions[index+1:]...)
}

// UpdateSession replaces the start and end times of the session at the given
// index.
func (s *Store) UpdateSession(index int, start time.Time, end *time.Time) error {
	if index < 0 || index >= len(s.Sessions) {
		return fmt.Errorf("session index out of range")
	}
	s.Sessions[index].Start = start
	s.Sessions[index].End = end
	return nil
}

// SortSessionsDesc sorts sessions by start time descending (newest first).
// This is the display order for the session list view — the most recent
// session is at the top, matching how users think about their recent work.
func (s *Store) SortSessionsDesc() {
	sort.SliceStable(s.Sessions, func(i, j int) bool {
		return s.Sessions[i].Start.After(s.Sessions[j].Start)
	})
}

func (s *Store) HasActive() bool {
	for _, st := range s.Streams {
		if st.Active {
			return true
		}
	}
	return false
}

// SortStreams sorts active streams to the top, then by creation time
// (oldest first). SliceStable is used so streams with equal state preserve
// their relative order, avoiding visual jitter in the TUI.
func (s *Store) SortStreams() {
	sort.SliceStable(s.Streams, func(i, j int) bool {
		ai, aj := s.Streams[i].Active, s.Streams[j].Active
		if ai != aj {
			return ai
		}
		return s.Streams[i].CreatedAt.Before(s.Streams[j].CreatedAt)
	})
}
