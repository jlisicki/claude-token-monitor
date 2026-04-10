package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
	"token-monitor/tui"
	"token-monitor/watcher"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alecthomas/kong"
)

// Globals holds flags shared across all commands.
type Globals struct {
	Path string `short:"p" default:"${default_path}" help:"Path to Claude Code projects directory." type:"existingdir"`
}

var cli struct {
	Globals

	Dashboard DashboardCmd `cmd:"" help:"Interactive TUI dashboard." default:"withargs"`
	Tail      TailCmd      `cmd:"" help:"Stream token usage lines in real-time."`
	Top       TopCmd       `cmd:"" help:"Live process table sorted by cost."`
	Summary   SummaryCmd   `cmd:"" help:"Print summary and exit."`
}

type DashboardCmd struct{}

type TailCmd struct {
	Debug   bool `short:"d" help:"Print raw JSONL line for each record."`
	Verbose bool `short:"v" help:"Also show user/system lines."`
	History bool `short:"H" help:"Include historical summary on startup."`
}

type TopCmd struct {
	Window time.Duration `short:"w" default:"5m" help:"Sliding window duration."`
	Expand bool          `short:"e" help:"Show each sub-agent on its own line."`
}

type SummaryCmd struct{}

func (cmd *DashboardCmd) Run(g *Globals) error {
	w := watcher.New(g.Path, false, false)
	s, err := w.InitialScan()
	if err != nil {
		return fmt.Errorf("scanning files: %w", err)
	}
	s.KeepRecords = true

	w.Start()
	defer w.Stop()

	m := tui.NewModel(s, w.Records())
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func (cmd *TailCmd) Run(g *Globals) error {
	runTail(g.Path, cmd.Debug, cmd.Verbose, cmd.History)
	return nil
}

func (cmd *TopCmd) Run(g *Globals) error {
	runTop(g.Path, cmd)
	return nil
}

func (cmd *SummaryCmd) Run(g *Globals) error {
	runSummary(g.Path)
	return nil
}

func main() {
	ctx := kong.Parse(&cli,
		kong.Name("token-monitor"),
		kong.Description("Monitor Claude Code token usage and costs."),
		kong.Vars{"default_path": defaultProjectsPath()},
		kong.DefaultEnvars("TOKEN_MONITOR"),
		kong.Bind(&cli.Globals),
	)
	if err := ctx.Run(&cli.Globals); err != nil {
		ctx.FatalIfErrorf(err)
	}
}

func defaultProjectsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}
