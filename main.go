package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).MarginBottom(1)
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dotStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	helpStyle   = lipgloss.NewStyle().Faint(true).MarginTop(1)
)

// tickMsg drives the 1-second UI refresh loop. We use tea.Tick (which
// internally uses time.NewTimer) rather than a goroutine with time.Ticker
// because Bubble Tea's message-based architecture requires all state updates
// to flow through Update(). A raw goroutine would cause data races.
type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// model holds all TUI state. Store is a pointer (shared, mutable state)
// while everything else is value-type view state. pendingD implements
// vim-style "dd" delete: the first "d" sets pendingD, the second triggers
// the delete. Any other key resets it.
// startingAt tracks the "timed toggle" input mode, where the user types a
// backdated start time (minutes ago or HH:MM) before activating a stream.
// startingAtID remembers which stream to activate once the input is confirmed.
// startErr holds a parse error to display inline until the next keypress.
// viewSessions toggles between the stream list (default) and the session list.
// sessionCursor tracks the cursor position independently within the session
// list so switching views preserves each cursor's position.
type model struct {
	store        *Store
	cursor       int
	adding       bool
	addAbove     bool
	pendingD     bool
	confirmDel   bool
	startingAt       bool
	startingAtID     string
	startErr         string
	loggingPast      bool
	loggingPastStart *time.Time
	viewSessions        bool
	sessionCursor       int
	pendingSessionD     bool
	confirmSessionDel   bool
	editingSession      bool
	editingSessionStart *time.Time
	textinput    textinput.Model
	ticking      bool
	width        int
	height       int
}

func initialModel(store *Store) model {
	ti := textinput.New()
	ti.Placeholder = "Stream name"
	ti.CharLimit = 40

	store.SortStreams()
	return model{
		store:     store,
		textinput: ti,
		ticking:   store.HasActive(),
	}
}

func (m model) Init() tea.Cmd {
	if m.ticking {
		return tickCmd()
	}
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		if m.store.HasActive() {
			m.sortAndFollow()
			return m, tickCmd()
		}
		m.ticking = false
		return m, nil

	case tea.KeyMsg:
		if m.viewSessions {
			if m.confirmSessionDel {
				return m.updateConfirmSessionDel(msg)
			}
			if m.editingSession {
				return m.updateEditingSession(msg)
			}
			return m.updateSessionView(msg)
		}
		if m.confirmDel {
			return m.updateConfirmDel(msg)
		}
		if m.adding {
			return m.updateAdding(msg)
		}
		if m.startingAt {
			return m.updateStartingAt(msg)
		}
		if m.loggingPast {
			return m.updateLoggingPast(msg)
		}
		return m.updateNormal(msg)
	}

	return m, nil
}

func (m *model) cursorID() string {
	if len(m.store.Streams) == 0 || m.cursor >= len(m.store.Streams) {
		return ""
	}
	return m.store.Streams[m.cursor].ID
}

// sortAndFollow re-sorts the stream list and moves the cursor to follow the
// stream it was previously on. Without this, sorting would silently move the
// cursor to a different stream, which is disorienting — the user expects the
// highlight to stay on the same item even as the list reorders.
func (m *model) sortAndFollow() {
	id := m.cursorID()
	m.store.SortStreams()
	for i, s := range m.store.Streams {
		if s.ID == id {
			m.cursor = i
			return
		}
	}
}

func (m model) updateAdding(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		name := strings.TrimSpace(m.textinput.Value())
		if name != "" {
			pos := m.cursor + 1
			if m.addAbove {
				pos = m.cursor
			}
			if pos >= len(m.store.Streams) {
				pos = len(m.store.Streams)
			}
			m.store.AddStream(name, pos)
			if pos >= len(m.store.Streams) {
				pos = len(m.store.Streams) - 1
			}
			m.cursor = pos
			m.sortAndFollow()
			m.store.Save()
		}
		m.adding = false
		m.textinput.Reset()
		return m, nil
	case "esc":
		m.adding = false
		m.textinput.Reset()
		return m, nil
	}
	var cmd tea.Cmd
	m.textinput, cmd = m.textinput.Update(msg)
	return m, cmd
}

func (m model) updateStartingAt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		input := strings.TrimSpace(m.textinput.Value())
		if input == "" {
			m.startingAt = false
			m.textinput.Reset()
			return m, nil
		}
		startAt, err := parseStartTime(input)
		if err != nil {
			m.startErr = err.Error()
			return m, nil
		}
		m.store.ToggleStreamAt(m.startingAtID, startAt)
		m.sortAndFollow()
		m.store.Save()
		if !m.ticking && m.store.HasActive() {
			m.ticking = true
			m.startingAt = false
			m.textinput.Reset()
			return m, tickCmd()
		}
		m.startingAt = false
		m.textinput.Reset()
		return m, nil
	case "esc":
		m.startingAt = false
		m.startErr = ""
		m.textinput.Reset()
		return m, nil
	}
	m.startErr = ""
	var cmd tea.Cmd
	m.textinput, cmd = m.textinput.Update(msg)
	return m, cmd
}

// parseStartTime interprets the user's input as either minutes ago (plain
// number) or an absolute HH:MM time (contains ':'). Returns the resolved
// time.Time or an error for invalid input.
func parseStartTime(input string) (time.Time, error) {
	if strings.Contains(input, ":") {
		t, err := time.Parse("15:04", input)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid time format, use HH:MM")
		}
		now := time.Now()
		startAt := time.Date(now.Year(), now.Month(), now.Day(),
			t.Hour(), t.Minute(), 0, 0, now.Location())
		if startAt.After(now) {
			return time.Time{}, fmt.Errorf("time is in the future")
		}
		return startAt, nil
	}
	mins, err := strconv.Atoi(input)
	if err != nil || mins < 0 {
		return time.Time{}, fmt.Errorf("enter a number of minutes or HH:MM")
	}
	return time.Now().Add(-time.Duration(mins) * time.Minute), nil
}

// updateLoggingPast handles two-phase input for logging a completed past time
// block. Phase 1 collects the start time; phase 2 collects the end time and
// validates that end > start before committing the block via AddPastTime.
func (m model) updateLoggingPast(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		input := strings.TrimSpace(m.textinput.Value())
		if input == "" {
			m.loggingPast = false
			m.loggingPastStart = nil
			m.textinput.Reset()
			return m, nil
		}
		parsed, err := parseStartTime(input)
		if err != nil {
			m.startErr = err.Error()
			return m, nil
		}
		if m.loggingPastStart == nil {
			// Phase 1: start time collected, now ask for end time.
			m.loggingPastStart = &parsed
			m.startErr = ""
			m.textinput.Reset()
			m.textinput.Placeholder = "End time (minutes ago or HH:MM)"
			return m, nil
		}
		// Phase 2: end time collected, validate and commit.
		end := parsed
		start := *m.loggingPastStart
		if !end.After(start) {
			m.startErr = "end time must be after start time"
			return m, nil
		}
		m.store.AddPastTime(start, end)
		m.sortAndFollow()
		m.store.Save()
		m.loggingPast = false
		m.loggingPastStart = nil
		m.startErr = ""
		m.textinput.Reset()
		return m, nil
	case "esc":
		m.loggingPast = false
		m.loggingPastStart = nil
		m.startErr = ""
		m.textinput.Reset()
		return m, nil
	}
	m.startErr = ""
	var cmd tea.Cmd
	m.textinput, cmd = m.textinput.Update(msg)
	return m, cmd
}

// updateSessionView handles navigation and actions within the session list.
// j/k move the cursor, dd initiates deletion (with confirmation), enter starts
// editing a session's times, and v/esc return to the stream view.
func (m model) updateSessionView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() != "d" {
		m.pendingSessionD = false
	}
	switch msg.String() {
	case "q", "ctrl+c":
		m.store.Save()
		return m, tea.Quit

	case "j", "down":
		if len(m.store.Sessions) > 0 {
			m.sessionCursor = (m.sessionCursor + 1) % len(m.store.Sessions)
		}
		return m, nil

	case "k", "up":
		if len(m.store.Sessions) > 0 {
			m.sessionCursor = (m.sessionCursor - 1 + len(m.store.Sessions)) % len(m.store.Sessions)
		}
		return m, nil

	case "d":
		if len(m.store.Sessions) == 0 {
			return m, nil
		}
		if !m.pendingSessionD {
			m.pendingSessionD = true
			return m, nil
		}
		// dd: always confirm since sessions have time data.
		m.pendingSessionD = false
		m.confirmSessionDel = true
		return m, nil

	case "enter":
		if len(m.store.Sessions) == 0 {
			return m, nil
		}
		sess := m.store.Sessions[m.sessionCursor]
		m.editingSession = true
		m.editingSessionStart = nil
		m.startErr = ""
		m.textinput.Reset()
		// Show current start time as placeholder for context.
		m.textinput.Placeholder = fmt.Sprintf("Start HH:MM (current: %s)", sess.Start.Format("15:04"))
		m.textinput.Focus()
		return m, textinput.Blink

	case "v", "esc":
		m.viewSessions = false
		return m, nil
	}
	return m, nil
}

// updateConfirmSessionDel handles the y/n confirmation prompt when deleting a
// session. Deletion is always safe for validate() because removing a session
// reduces wall-clock time, which can only make the invariant easier to satisfy.
func (m model) updateConfirmSessionDel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		m.confirmSessionDel = false
		m.store.DeleteSession(m.sessionCursor)
		m.store.SortSessionsDesc()
		m.store.Save()
		if m.sessionCursor >= len(m.store.Sessions) && m.sessionCursor > 0 {
			m.sessionCursor--
		}
		return m, nil
	default:
		m.confirmSessionDel = false
		return m, nil
	}
}

// updateEditingSession handles two-phase time editing for a session.
// Phase 1 collects the new start time; phase 2 collects the new end time.
// Times are entered as HH:MM and anchored to the session's original date so
// the user doesn't need to re-enter the date. For ongoing sessions (End==nil),
// only the start time is edited — the session remains open.
// After applying the edit, validate() is run; on failure the edit is reverted
// and an error is shown.
func (m model) updateEditingSession(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		input := strings.TrimSpace(m.textinput.Value())
		sess := m.store.Sessions[m.sessionCursor]

		if m.editingSessionStart == nil {
			// Phase 1: collecting start time.
			if input == "" {
				// Empty input keeps the original start time.
				m.editingSessionStart = &sess.Start
			} else {
				t, err := time.Parse("15:04", input)
				if err != nil {
					m.startErr = "invalid time format, use HH:MM"
					return m, nil
				}
				// Anchor to the session's original date.
				newStart := time.Date(sess.Start.Year(), sess.Start.Month(), sess.Start.Day(),
					t.Hour(), t.Minute(), 0, 0, sess.Start.Location())
				m.editingSessionStart = &newStart
			}

			// For ongoing sessions, skip end time phase — apply immediately.
			if sess.End == nil {
				newStart := *m.editingSessionStart
				if err := m.store.UpdateSession(m.sessionCursor, newStart, nil); err != nil {
					m.startErr = "invalid: " + err.Error()
					m.editingSessionStart = nil
					return m, nil
				}
				m.store.SortSessionsDesc()
				m.store.Save()
				m.editingSession = false
				m.editingSessionStart = nil
				m.startErr = ""
				m.textinput.Reset()
				return m, nil
			}

			// Move to phase 2: end time.
			m.startErr = ""
			m.textinput.Reset()
			m.textinput.Placeholder = fmt.Sprintf("End HH:MM (current: %s)", sess.End.Format("15:04"))
			return m, nil
		}

		// Phase 2: collecting end time.
		newStart := *m.editingSessionStart
		var newEnd *time.Time
		if input == "" {
			// Empty input keeps the original end time.
			newEnd = sess.End
		} else {
			t, err := time.Parse("15:04", input)
			if err != nil {
				m.startErr = "invalid time format, use HH:MM"
				return m, nil
			}
			// Anchor end to the original end date (may differ from start date
			// for sessions that cross midnight).
			endTime := time.Date(sess.End.Year(), sess.End.Month(), sess.End.Day(),
				t.Hour(), t.Minute(), 0, 0, sess.End.Location())
			newEnd = &endTime
		}

		if newEnd != nil && !newEnd.After(newStart) {
			m.startErr = "end time must be after start time"
			return m, nil
		}

		if err := m.store.UpdateSession(m.sessionCursor, newStart, newEnd); err != nil {
			m.startErr = "invalid: " + err.Error()
			return m, nil
		}
		m.store.SortSessionsDesc()
		m.store.Save()
		m.editingSession = false
		m.editingSessionStart = nil
		m.startErr = ""
		m.textinput.Reset()
		return m, nil

	case "esc":
		m.editingSession = false
		m.editingSessionStart = nil
		m.startErr = ""
		m.textinput.Reset()
		return m, nil
	}
	m.startErr = ""
	var cmd tea.Cmd
	m.textinput, cmd = m.textinput.Update(msg)
	return m, cmd
}

func (m model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() != "d" {
		m.pendingD = false
	}
	switch msg.String() {
	case "q", "ctrl+c":
		// Save with streams still active so they resume on next launch.
		// Quitting is not the same as stopping work.
		m.store.Save()
		return m, tea.Quit

	case "j", "down", "ctrl+j":
		if len(m.store.Streams) > 0 {
			m.cursor = (m.cursor + 1) % len(m.store.Streams)
		}
		m.pendingD = false
		return m, nil

	case "k", "up", "ctrl+k":
		if len(m.store.Streams) > 0 {
			m.cursor = (m.cursor - 1 + len(m.store.Streams)) % len(m.store.Streams)
		}
		m.pendingD = false
		return m, nil

	case "o":
		m.adding = true
		m.addAbove = false
		m.textinput.Focus()
		return m, textinput.Blink

	case "O":
		m.adding = true
		m.addAbove = true
		m.textinput.Focus()
		return m, textinput.Blink

	case "enter", " ":
		if len(m.store.Streams) == 0 {
			return m, nil
		}
		m.store.ToggleStream(m.store.Streams[m.cursor].ID)
		m.sortAndFollow()
		m.store.Save()
		if !m.ticking && m.store.HasActive() {
			m.ticking = true
			return m, tickCmd()
		}
		if !m.store.HasActive() {
			m.ticking = false
		}
		return m, nil

	case "s":
		m.store.StopAll()
		m.sortAndFollow()
		m.store.Save()
		m.ticking = false
		return m, nil

	case "c":
		m.store.ContinueAll()
		m.sortAndFollow()
		m.store.Save()
		if !m.ticking && m.store.HasActive() {
			m.ticking = true
			return m, tickCmd()
		}
		return m, nil

	case "d":
		if !m.pendingD {
			m.pendingD = true
			return m, nil
		}
		// dd: delete
		m.pendingD = false
		if len(m.store.Streams) == 0 {
			return m, nil
		}
		m.confirmDel = true
		return m, nil

	case "t":
		if len(m.store.Streams) == 0 {
			return m, nil
		}
		stream := m.store.Streams[m.cursor]
		if stream.Active {
			// If already active, just deactivate — backdating doesn't apply.
			m.store.ToggleStream(stream.ID)
			m.sortAndFollow()
			m.store.Save()
			if !m.store.HasActive() {
				m.ticking = false
			}
			return m, nil
		}
		// Enter timed-start input mode.
		m.startingAt = true
		m.startingAtID = stream.ID
		m.startErr = ""
		m.textinput.Placeholder = "Minutes ago or HH:MM"
		m.textinput.Focus()
		return m, textinput.Blink

	case "T":
		// Enter "log past time" input mode: two sequential prompts for
		// start and end time. A closed session is added directly.
		m.loggingPast = true
		m.loggingPastStart = nil
		m.startErr = ""
		m.textinput.Placeholder = "Start time (minutes ago or HH:MM)"
		m.textinput.Focus()
		return m, textinput.Blink

	case "v":
		m.viewSessions = true
		m.store.SortSessionsDesc()
		m.sessionCursor = 0
		return m, nil

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		n := int(msg.String()[0] - '1')
		if n < len(m.store.Streams) {
			m.cursor = n
		}
		return m, nil
	}
	return m, nil
}

func (m model) updateConfirmDel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		m.confirmDel = false
		return m.performDelete()
	default:
		m.confirmDel = false
		return m, nil
	}
}

// performDelete removes the stream at the cursor. If the stream was active,
// we deactivate it first (without flushing — the time is being discarded with
// the stream) and check whether that was the last active stream so we can
// close the wall-clock session.
func (m model) performDelete() (tea.Model, tea.Cmd) {
	if len(m.store.Streams) == 0 {
		return m, nil
	}
	stream := m.store.Streams[m.cursor]
	wasActive := stream.Active
	if wasActive {
		m.store.Streams[m.cursor].Active = false
		m.store.Streams[m.cursor].StartedAt = nil
	}
	m.store.DeleteStream(stream.ID)
	if wasActive && !m.store.HasActive() {
		m.store.closeCurrentSession()
	}
	m.store.SortStreams()
	m.store.Save()
	// Clamp cursor so it doesn't point past the end of the list.
	if m.cursor >= len(m.store.Streams) && m.cursor > 0 {
		m.cursor--
	}
	if !m.store.HasActive() {
		m.ticking = false
	}
	return m, nil
}

func formatDuration(total time.Duration) string {
	s := int(total.Seconds())
	h := s / 3600
	min := (s % 3600) / 60
	sec := s % 60
	return fmt.Sprintf("%dh %02dm %02ds", h, min, sec)
}

func (m model) View() string {
	if m.viewSessions {
		return m.viewSessionList()
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("urd - Time Tracker"))
	b.WriteString("\n\n")

	if len(m.store.Streams) == 0 && !m.adding {
		// Box-drawn empty state gives visual weight to the onboarding hint,
		// making the first-launch experience feel intentional rather than broken.
		boxStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(1, 2)
		b.WriteString(boxStyle.Render("No streams yet.\nPress 'o' to start.") + "\n")
	}

	for i, s := range m.store.Streams {
		cursor := "  "
		if i == m.cursor {
			cursor = cursorStyle.Render("> ")
		}

		num := lipgloss.NewStyle().Faint(true).Render(fmt.Sprintf("%d ", i+1))

		line := fmt.Sprintf("%-20s", s.Name)
		if s.Active {
			line += "  " + dotStyle.Render("●")
		}
		b.WriteString(cursor + num + line + "\n")
	}

	if m.adding {
		b.WriteString("\n  " + m.textinput.View() + "\n")
	}

	if m.startingAt {
		b.WriteString("\n  Start time: " + m.textinput.View() + "\n")
		if m.startErr != "" {
			errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
			b.WriteString("  " + errStyle.Render(m.startErr) + "\n")
		}
	}

	if m.loggingPast {
		label := "Start time: "
		if m.loggingPastStart != nil {
			label = "End time: "
		}
		b.WriteString("\n  " + label + m.textinput.View() + "\n")
		if m.startErr != "" {
			errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
			b.WriteString("  " + errStyle.Render(m.startErr) + "\n")
		}
	}

	if m.confirmDel {
		warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
		name := m.store.Streams[m.cursor].Name
		b.WriteString("\n  " + warnStyle.Render(fmt.Sprintf("Delete \"%s\"? (y/n)", name)) + "\n")
	}

	b.WriteString("\n")

	total := m.store.TotalWallClock()
	if total > 0 || m.store.HasActive() {
		dimStyle := lipgloss.NewStyle().Faint(true)
		fmt.Fprintf(&b, "  %s\n", dimStyle.Render(fmt.Sprintf("Wall clock: %s", formatDuration(total))))
	}

	b.WriteString(helpStyle.Render("\n  o/O add below/above · enter toggle · t timed start · T log past · dd delete · s stop all · c continue · v sessions · q quit"))

	return b.String()
}

// viewSessionList renders the session list view. Each row shows the date,
// start/end time (or "..." for ongoing), duration, and an active indicator.
// The cursor highlights the selected row for editing or deletion.
func (m model) viewSessionList() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("urd - Sessions"))
	b.WriteString("\n\n")

	if len(m.store.Sessions) == 0 {
		dimStyle := lipgloss.NewStyle().Faint(true)
		b.WriteString("  " + dimStyle.Render("No sessions recorded yet.") + "\n")
	}

	for i, sess := range m.store.Sessions {
		cursor := "  "
		if i == m.sessionCursor {
			cursor = cursorStyle.Render("> ")
		}

		date := sess.Start.Format("2006-01-02")
		startTime := sess.Start.Format("15:04")
		endTime := "..."
		if sess.End != nil {
			endTime = sess.End.Format("15:04")
		}

		var dur time.Duration
		if sess.End != nil {
			dur = sess.End.Sub(sess.Start)
		} else {
			dur = time.Since(sess.Start).Truncate(time.Second)
		}

		line := fmt.Sprintf("%s  %s - %-5s   (%s)", date, startTime, endTime, formatDuration(dur))
		if sess.End == nil {
			line += "  " + dotStyle.Render("●")
		}

		b.WriteString(cursor + line + "\n")
	}

	if m.editingSession {
		label := "Start time: "
		if m.editingSessionStart != nil {
			label = "End time: "
		}
		b.WriteString("\n  " + label + m.textinput.View() + "\n")
		if m.startErr != "" {
			errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
			b.WriteString("  " + errStyle.Render(m.startErr) + "\n")
		}
	}

	if m.confirmSessionDel {
		warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
		sess := m.store.Sessions[m.sessionCursor]
		b.WriteString("\n  " + warnStyle.Render(fmt.Sprintf(
			"Delete session %s %s? (y/n)",
			sess.Start.Format("2006-01-02"),
			sess.Start.Format("15:04"),
		)) + "\n")
	}

	b.WriteString(helpStyle.Render("\n  j/k navigate · dd delete · enter edit · v back · q quit"))

	return b.String()
}

func main() {
	store, err := LoadStore("urd.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading data: %v\n", err)
		os.Exit(1)
	}

	// WithAltScreen so the TUI doesn't pollute the user's scroll-back buffer
	// — on exit, the terminal is restored to its previous state.
	p := tea.NewProgram(initialModel(store), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
