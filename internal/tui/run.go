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
		title   string
		history []turn
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
	}

	// Detect the terminal's background colour now, while we still own stdin in
	// cooked mode. lipgloss caches it; otherwise it queries (OSC 11) on first
	// render — once bubbletea holds stdin in raw mode — and the terminal's reply
	// races the input reader and prints as garbage (`]11;rgb:…`) in the chat.
	_ = lipgloss.HasDarkBackground()

	// No WithMouseCellMotion: capturing the mouse steals click-drag from the
	// terminal, so text can't be selected/copied. pgup/pgdn/ctrl+u/ctrl+d scroll.
	p := tea.NewProgram(
		New(ctx, c, chatID, title, history, initialPrompt, serverLabel),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	_, err := p.Run()
	return err
}
