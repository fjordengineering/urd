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

type Stream struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Seconds   int64      `json:"seconds"`
	Active    bool       `json:"active"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type Session struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
}

type Store struct {
	Streams    []Stream  `json:"streams"`
	Sessions   []Session `json:"sessions"`
	LastActive []string  `json:"last_active,omitempty"`
	FilePath   string    `json:"-"`
}

func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

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
	// Safety fallback: if a stream is active but has no StartedAt, fix it.
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
	// Only count closed sessions. Open sessions represent live time that
	// hasn't been flushed to stream seconds yet (e.g. after a crash).
	var wallClock time.Duration
	for _, sess := range s.Sessions {
		if sess.End != nil {
			wallClock += sess.End.Sub(sess.Start)
		}
	}
	wallClock = wallClock.Truncate(time.Second)
	// Allow 1s tolerance per closed session for flushStream truncation.
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

func (s *Store) ToggleStream(id string) {
	hadActive := s.HasActive()
	for i := range s.Streams {
		if s.Streams[i].ID == id {
			if s.Streams[i].Active {
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
		// Went from 0 active to 1+: start a new wall-clock session
		s.Sessions = append(s.Sessions, Session{Start: time.Now()})
	} else if hadActive && !hasActive {
		// Went from 1+ active to 0: close the current session
		s.closeCurrentSession()
	}
}

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

func (s *Store) flushStream(st *Stream) {
	if st.StartedAt != nil {
		st.Seconds += int64(time.Since(*st.StartedAt).Seconds())
	}
}

func (s *Store) SaveForBackground() error {
	for i := range s.Streams {
		if s.Streams[i].Active && s.Streams[i].StartedAt != nil {
			elapsed := time.Since(*s.Streams[i].StartedAt)
			whole := elapsed.Truncate(time.Second)
			s.Streams[i].Seconds += int64(whole / time.Second)
			// Advance StartedAt by only the whole seconds so the
			// sub-second remainder isn't lost on each background save.
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
func (st *Stream) Elapsed() time.Duration {
	d := time.Duration(st.Seconds) * time.Second
	if st.Active && st.StartedAt != nil {
		d += time.Since(*st.StartedAt).Truncate(time.Second)
	}
	return d
}
