package tui

import (
	"fmt"
	"strings"
	"time"
	"token-monitor/model"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205")).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			Padding(0, 1)

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).
			Width(14).
			Align(lipgloss.Right)

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("156")).
			Width(14).
			Align(lipgloss.Right)

	costStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220")).
			Width(12).
			Align(lipgloss.Right)

	totalStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("255"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(1, 2)
)

type recordsMsg []model.TokenRecord
type tickMsg time.Time

type Model struct {
	summary *model.Summary
	records <-chan []model.TokenRecord
	width   int
	height  int
}

func NewModel(summary *model.Summary, records <-chan []model.TokenRecord) Model {
	return Model{
		summary: summary,
		records: records,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitForRecords(m.records), tickCmd())
}

func waitForRecords(ch <-chan []model.TokenRecord) tea.Cmd {
	return func() tea.Msg {
		recs, ok := <-ch
		if !ok {
			return nil
		}
		return recordsMsg(recs)
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case recordsMsg:
		for _, r := range msg {
			m.summary.Add(r)
		}
		return m, waitForRecords(m.records)
	case tickMsg:
		return m, tickCmd()
	}
	return m, nil
}

func (m Model) View() string {
	s := m.summary
	var b strings.Builder

	b.WriteString(titleStyle.Render("⚡ Claude Code Token Monitor"))
	b.WriteString("\n\n")

	b.WriteString(headerStyle.Render("Token Usage"))
	b.WriteString("\n")
	b.WriteString(renderRow("Input", s.TotalInputTokens(), s.TotalInputCost()))
	b.WriteString(renderSubRow("Cache read", s.CacheReadTokens, s.CostByTokenType("cache_read")))
	b.WriteString(renderSubRow("Cache write", s.CacheCreationTokens, s.CostByTokenType("cache_write")))
	b.WriteString(renderSubRow("Uncached", s.InputTokens, s.CostByTokenType("input")))
	b.WriteString(renderRow("Output", s.OutputTokens, s.CostByTokenType("output")))
	b.WriteString(renderRow("Thinking", s.ThinkingTokens, s.CostByTokenType("thinking")))
	b.WriteString(dimStyle.Render("  " + strings.Repeat("─", 42)))
	b.WriteString("\n")
	b.WriteString("  " + totalStyle.Render(
		fmt.Sprintf("%-14s %14s %12s",
			"Total",
			model.FormatTokens(s.TotalTokens),
			model.FormatCost(s.TotalCost),
		)))
	b.WriteString("\n\n")

	b.WriteString(headerStyle.Render("By Model"))
	b.WriteString("\n")
	models := model.SortedModels(s.ByModel)
	for _, ms := range models {
		b.WriteString(fmt.Sprintf("  %-14s %14s  %12s\n",
			model.DisplayName(ms.Model),
			model.FormatTokens(ms.TotalTokens),
			model.FormatCost(ms.Cost),
		))
	}
	b.WriteString("\n")

	b.WriteString(headerStyle.Render("Recent Activity"))
	b.WriteString("\n")
	windows := s.WindowSummaries(time.Now())
	for _, w := range windows {
		b.WriteString(fmt.Sprintf("  %-14s %14s  %12s\n",
			w.Label,
			model.FormatTokens(w.TotalTokens),
			model.FormatCost(w.TotalCost),
		))
	}
	b.WriteString("\n")

	ago := "never"
	if !s.LastUpdate.IsZero() {
		d := time.Since(s.LastUpdate)
		if d < time.Second {
			ago = "just now"
		} else {
			ago = fmt.Sprintf("%ds ago", int(d.Seconds()))
		}
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf(
		"  Sessions: %d  │  Updated: %s  │  q to quit",
		s.SessionCount, ago,
	)))
	b.WriteString("\n")

	return borderStyle.Render(b.String())
}

func renderRow(label string, tokens int, cost float64) string {
	return fmt.Sprintf("  %s %s %s\n",
		labelStyle.Render(label),
		valueStyle.Render(model.FormatTokens(tokens)),
		costStyle.Render(model.FormatCost(cost)),
	)
}

func renderSubRow(label string, tokens int, cost float64) string {
	return fmt.Sprintf("  %s %s %s\n",
		dimStyle.Copy().Width(14).Align(lipgloss.Right).Render(label),
		dimStyle.Copy().Width(14).Align(lipgloss.Right).Render(model.FormatTokens(tokens)),
		dimStyle.Copy().Width(12).Align(lipgloss.Right).Render(model.FormatCost(cost)),
	)
}
