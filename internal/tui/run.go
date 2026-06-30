package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fagerbergj/quack/internal/cli"
)

// Run opens the interactive chat TUI. A blank chatID creates a new chat;
// otherwise it resumes that chat, loading its transcript first. initialPrompt, if
// set, is auto-sent on start. Create/load errors surface here, before the
// alt-screen takes over (so they print as plain errors, not lost behind the UI).
func Run(ctx context.Context, c *cli.Client, chatID, initialPrompt, serverLabel string) error {
	var (
		title        string
		history      []turn
		resume       bool
		resumePrompt string
	)
	if chatID == "" {
		id, err := c.CreateChat(ctx, "")
		if err != nil {
			return err
		}
		chatID = id
	} else {
		detail, err := c.GetChat(ctx, chatID)
		if err != nil {
			if errors.Is(err, cli.ErrNotFound) {
				return fmt.Errorf("chat %s not found", chatID)
			}
			return err
		}
		if detail.Title != nil {
			title = *detail.Title
		}
		for _, t := range detail.Turns {
			history = append(history, turn{
				user:   strings.TrimSpace(t.Input.Content),
				answer: strings.TrimSpace(cli.AssistantText(t.Output)),
			})
		}
		// If the latest run is still in progress, its DAG/answer aren't in the
		// persisted assistant text (AssistantText drops them) — re-attach to the live
		// stream instead. Lift that turn out of history so it renders once, live.
		if n := len(detail.Turns); n > 0 && cli.DagInProgress(detail.Turns[n-1].Output) {
			resume = true
			resumePrompt = strings.TrimSpace(detail.Turns[n-1].Input.Content)
			history = history[:len(history)-1]
		}
	}

	// Detect the terminal's background colour now, while we still own stdin in
	// cooked mode. lipgloss caches it; otherwise it queries (OSC 11) on first
	// render — once bubbletea holds stdin in raw mode — and the terminal's reply
	// races the input reader and prints as garbage (`]11;rgb:…`) in the chat.
	_ = lipgloss.HasDarkBackground()

	m := New(ctx, c, chatID, title, history, initialPrompt, serverLabel)
	if resume {
		m.resume = true
		m.pending = resumePrompt // the live user bubble; startResume keeps it
	}

	// No WithMouseCellMotion: capturing the mouse steals click-drag from the
	// terminal, so text can't be selected/copied. pgup/pgdn/ctrl+u/ctrl+d scroll.
	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	_, err := p.Run()
	return err
}
