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

// Stream represents a named time-tracking category. Time is tracked using a
// two-part scheme: Seconds stores the cumulative flushed time as whole seconds,
// while StartedAt marks when the current active period began. This split lets
// us persist progress to disk (via Seconds) without stopping the timer, while
// StartedAt provides the live component that Elapsed() adds on top.
// Seconds is int64 rather than float64 because sub-second precision isn't
// meaningful for a human time tracker and whole seconds avoid floating-point
// drift across repeated flush cycles.
type Stream struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Seconds   int64      `json:"seconds"`
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
	if err := s.validate(); err != nil {
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

func (s *Store) validate() error {
	var streamTotal time.Duration
	for _, st := range s.Streams {
		streamTotal += time.Duration(st.Seconds) * time.Second
	}
	// Only count closed sessions for validation. Open sessions represent
	// live time that hasn't been flushed to stream seconds yet (e.g. after
	// a crash or force-quit).
	var wallClock time.Duration
	for _, sess := range s.Sessions {
		if sess.End != nil {
			wallClock += sess.End.Sub(sess.Start)
		}
	}
	wallClock = wallClock.Truncate(time.Second)
	// Allow a tolerance of 1 second per closed session to account for
	// truncation in flushStream (int64 conversion drops sub-second parts).
	closedCount := 0
	for _, sess := range s.Sessions {
		if sess.End != nil {
			closedCount++
		}
	}
	tolerance := time.Duration(closedCount) * time.Second
	if wallClock > 0 && streamTotal+tolerance < wallClock {
		return fmt.Errorf(
			"inconsistent data in %s: total stream time (%s) is less than wall-clock time (%s)",
			s.FilePath, streamTotal, wallClock,
		)
	}
	return nil
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

// ToggleStream activates or deactivates a single stream by ID.
// Session management is edge-triggered: we only open/close a wall-clock
// session when the count of active streams crosses zero. This means
// switching between streams (deactivate A, activate B) doesn't create a
// gap in the wall-clock session — only going from "nothing active" to
// "something active" (or vice versa) triggers a session boundary.
func (s *Store) ToggleStream(id string) {
	hadActive := s.HasActive()
	for i := range s.Streams {
		if s.Streams[i].ID == id {
			if s.Streams[i].Active {
				// Deactivating: flush elapsed time to Seconds and clear
				// StartedAt so we stop accumulating live time.
				s.flushStream(&s.Streams[i])
				s.Streams[i].Active = false
				s.Streams[i].StartedAt = nil
			} else {
				now := time.Now()
				s.Streams[i].Active = true
				s.Streams[i].StartedAt = &now
			}
			break
		}
	}
	hasActive := s.HasActive()
	if !hadActive && hasActive {
		s.Sessions = append(s.Sessions, Session{Start: time.Now()})
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
			s.flushStream(&s.Streams[i])
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

// flushStream converts the live elapsed time (since StartedAt) into the
// persisted Seconds field. The int64 conversion truncates sub-second
// precision, which is acceptable when stopping a stream. For periodic
// background saves, use SaveForBackground instead — it preserves the
// fractional remainder by adjusting StartedAt rather than resetting it.
func (s *Store) flushStream(st *Stream) {
	if st.StartedAt != nil {
		st.Seconds += int64(time.Since(*st.StartedAt).Seconds())
	}
}

// SaveForBackground persists stream progress while the app is running (called
// on quit). Unlike a full stop, streams stay Active so they resume on next
// launch. The key difference from flushStream is how StartedAt is handled:
// rather than resetting to time.Now() (which would discard the sub-second
// remainder on every save), we advance StartedAt by only the whole-second
// portion of the elapsed time. This prevents cumulative rounding drift that
// would cause stream seconds to fall behind wall-clock time.
func (s *Store) SaveForBackground() error {
	for i := range s.Streams {
		if s.Streams[i].Active && s.Streams[i].StartedAt != nil {
			elapsed := time.Since(*s.Streams[i].StartedAt)
			whole := elapsed.Truncate(time.Second)
			s.Streams[i].Seconds += int64(whole / time.Second)
			adjusted := s.Streams[i].StartedAt.Add(whole)
			s.Streams[i].StartedAt = &adjusted
		}
	}
	return s.Save()
}

func (s *Store) HasActive() bool {
	for _, st := range s.Streams {
		if st.Active {
			return true
		}
	}
	return false
}

// SortStreams sorts active streams to the top, then by elapsed time descending.
// SliceStable (not Slice) is used so streams with equal elapsed time preserve
// their relative order, avoiding visual jitter in the TUI.
func (s *Store) SortStreams() {
	sort.SliceStable(s.Streams, func(i, j int) bool {
		ai, aj := s.Streams[i].Active, s.Streams[j].Active
		if ai != aj {
			return ai
		}
		return s.Streams[i].Elapsed() > s.Streams[j].Elapsed()
	})
}

// Elapsed returns the total duration (stored + live) for a stream.
// The live portion is truncated to whole seconds so the displayed counter
// increments cleanly in 1-second steps rather than flickering with sub-second
// updates.
func (st *Stream) Elapsed() time.Duration {
	d := time.Duration(st.Seconds) * time.Second
	if st.Active && st.StartedAt != nil {
		d += time.Since(*st.StartedAt).Truncate(time.Second)
	}
	return d
}
