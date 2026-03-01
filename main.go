package main

import (
	"fmt"
	"os"
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
type model struct {
	store      *Store
	cursor     int
	adding     bool
	addAbove   bool
	pendingD   bool
	confirmDel bool
	textinput  textinput.Model
	ticking    bool
	width      int
	height     int
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
		if m.confirmDel {
			return m.updateConfirmDel(msg)
		}
		if m.adding {
			return m.updateAdding(msg)
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

func (m model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() != "d" {
		m.pendingD = false
	}
	switch msg.String() {
	case "q", "ctrl+c":
		// Save with streams still active so they resume on next launch.
		// We use SaveForBackground (not StopAll + Save) to preserve the
		// active state — quitting is not the same as stopping work.
		m.store.SaveForBackground()
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
		// If stream has recorded time, ask for confirmation
		if m.store.Streams[m.cursor].Elapsed() > 0 {
			m.confirmDel = true
			return m, nil
		}
		return m.performDelete()

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
	var b strings.Builder

	b.WriteString(titleStyle.Render("urd - Time Tracker"))
	b.WriteString("\n\n")

	// Left column: stream list
	var left strings.Builder
	if len(m.store.Streams) == 0 && !m.adding {
		left.WriteString("  No streams. Press 'o' to add one.\n")
	}

	total := m.store.TotalWallClock()
	totalSec := total.Seconds()

	for i, s := range m.store.Streams {
		cursor := "  "
		if i == m.cursor {
			cursor = cursorStyle.Render("> ")
		}

		num := lipgloss.NewStyle().Faint(true).Render(fmt.Sprintf("%d ", i+1))

		pctStr := ""
		if totalSec > 0 {
			pct := s.Elapsed().Seconds() / totalSec * 100
			pctStr = fmt.Sprintf("  %5.1f%%", pct)
		}

		line := fmt.Sprintf("%-20s %s%s", s.Name, formatDuration(s.Elapsed()), pctStr)
		if s.Active {
			line += "  " + dotStyle.Render("●")
		}
		b.WriteString(cursor + num + line + "\n")
	}

	if m.adding {
		b.WriteString("\n  " + m.textinput.View() + "\n")
	}

	if m.confirmDel {
		warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
		name := m.store.Streams[m.cursor].Name
		b.WriteString("\n  " + warnStyle.Render(fmt.Sprintf("Delete \"%s\"? It has recorded time. (y/n)", name)) + "\n")
	}

	b.WriteString("\n")

	if total > 0 || m.store.HasActive() {
		var sumStreams time.Duration
		for _, s := range m.store.Streams {
			sumStreams += s.Elapsed()
		}
		dimStyle := lipgloss.NewStyle().Faint(true)
		b.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(fmt.Sprintf("Wall clock: %s", formatDuration(total)))))
		b.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(fmt.Sprintf("Total:      %s", formatDuration(sumStreams)))))
	}

	b.WriteString(helpStyle.Render("\n  o/O add below/above · enter toggle · dd delete · s stop all · c continue · q quit"))

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
