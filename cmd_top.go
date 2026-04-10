package main

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
	"token-monitor/model"
	"token-monitor/parser"
	"token-monitor/watcher"
)

// recordKey stores records per session+agentID for full granularity.
type recordKey struct {
	SessionID string
	AgentID   string // empty = main agent
}

// sessionRow holds aggregated stats for one agent within a session.
type sessionRow struct {
	AgentID     string // empty for main, set for individual subagents
	RecordCount int
	CtxSize     int // total input tokens from the most recent API call
	Input       int
	Output      int
	Thinking    int
	CacheRead   int
	CacheWrite  int
	TotalTokens int
	CostInput   float64
	CostOutput  float64
	CostThink   float64
	CostCRead   float64
	CostCWrite  float64
	CostTotal   float64
}

// sessionGroup groups a session's main agent row with its subagent rows.
type sessionGroup struct {
	SessionID    string
	Project      string
	Main         *sessionRow          // nil if no main-agent records in window
	Subagents    []*sessionRow        // one merged row, or one per agentID when expanded
	CostTotal    float64              // main + all subagents combined
	Profile      model.SessionProfile // detected usage profile for context thresholds
}

type topState struct {
	mu       sync.Mutex
	rows     map[recordKey][]model.TokenRecord
	projects map[string]string // sessionID -> project name
	window   time.Duration
	allTime  *model.Summary
	dirty    bool
}

func newTopState(window time.Duration) *topState {
	return &topState{
		rows:     make(map[recordKey][]model.TokenRecord),
		projects: make(map[string]string),
		window:   window,
		allTime:  model.NewSummary(),
	}
}

func (ts *topState) add(r model.TokenRecord) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.allTime.Add(r)
	ts.addToRows(r)
}

func (ts *topState) addToRows(r model.TokenRecord) {
	key := recordKey{SessionID: r.SessionID, AgentID: r.AgentID}
	ts.rows[key] = append(ts.rows[key], r)
	if r.Project != "" {
		ts.projects[r.SessionID] = r.Project
	}
	ts.dirty = true
}

func (ts *topState) seed(records []model.TokenRecord, windowDur time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	cutoff := time.Now().Add(-windowDur)
	for _, r := range records {
		ts.allTime.Add(r)
		if !r.Timestamp.Before(cutoff) {
			ts.addToRows(r)
		}
	}
	ts.dirty = true
}

func aggregateRow(agentID string, records []model.TokenRecord) *sessionRow {
	row := &sessionRow{AgentID: agentID, RecordCount: len(records)}
	for _, r := range records {
		row.Input += r.InputTokens
		row.Output += r.OutputTokens
		row.Thinking += r.ThinkingTokens
		row.CacheRead += r.CacheReadTokens
		row.CacheWrite += r.CacheCreationTokens
		row.TotalTokens += r.TotalTokens()
		costs := model.CostsForRecord(r)
		row.CostInput += costs.Input
		row.CostOutput += costs.Output
		row.CostThink += costs.Thinking
		row.CostCRead += costs.CacheRead
		row.CostCWrite += costs.CacheWrite
		row.CostTotal += costs.Total
	}
	// Context size = total input tokens from the most recent call
	if len(records) > 0 {
		row.CtxSize = records[len(records)-1].TotalInputTokens()
	}
	return row
}

func mergeRows(rows []*sessionRow) *sessionRow {
	merged := &sessionRow{}
	for _, r := range rows {
		merged.RecordCount += r.RecordCount
		merged.Input += r.Input
		merged.Output += r.Output
		merged.Thinking += r.Thinking
		merged.CacheRead += r.CacheRead
		merged.CacheWrite += r.CacheWrite
		merged.TotalTokens += r.TotalTokens
		merged.CostInput += r.CostInput
		merged.CostOutput += r.CostOutput
		merged.CostThink += r.CostThink
		merged.CostCRead += r.CostCRead
		merged.CostCWrite += r.CostCWrite
		merged.CostTotal += r.CostTotal
		if r.CtxSize > merged.CtxSize {
			merged.CtxSize = r.CtxSize
		}
	}
	return merged
}

func (ts *topState) snapshot(now time.Time, expand bool) []sessionGroup {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	cutoff := now.Add(-ts.window)

	// Intermediate: collect per-agentID rows, grouped by session
	type sessionData struct {
		project    string
		main       *sessionRow
		subagents  []*sessionRow
		allRecords []model.TokenRecord // for profile detection
	}
	sessions := make(map[string]*sessionData)

	for key, records := range ts.rows {
		// Prune old records
		start := 0
		for start < len(records) && records[start].Timestamp.Before(cutoff) {
			start++
		}
		if start > 0 {
			kept := make([]model.TokenRecord, len(records)-start)
			copy(kept, records[start:])
			ts.rows[key] = kept
			records = kept
		}
		if len(records) == 0 {
			delete(ts.rows, key)
			continue
		}

		row := aggregateRow(key.AgentID, records)

		sd, ok := sessions[key.SessionID]
		if !ok {
			sd = &sessionData{project: ts.projects[key.SessionID]}
			sessions[key.SessionID] = sd
		}
		sd.allRecords = append(sd.allRecords, records...)

		if key.AgentID == "" {
			sd.main = row
		} else {
			sd.subagents = append(sd.subagents, row)
		}
	}

	ts.dirty = false

	// Build groups
	result := make([]sessionGroup, 0, len(sessions))
	for sid, sd := range sessions {
		g := sessionGroup{
			SessionID: sid,
			Project:   sd.project,
			Main:      sd.main,
			Profile:   model.DetectProfile(sd.allRecords),
		}
		if sd.main != nil {
			g.CostTotal += sd.main.CostTotal
		}
		for _, s := range sd.subagents {
			g.CostTotal += s.CostTotal
		}

		if expand {
			// Keep each subagent as its own line
			sort.Slice(sd.subagents, func(i, j int) bool {
				return sd.subagents[i].CostTotal > sd.subagents[j].CostTotal
			})
			g.Subagents = sd.subagents
		} else if len(sd.subagents) > 0 {
			// Merge all subagents into one line
			g.Subagents = []*sessionRow{mergeRows(sd.subagents)}
		}

		result = append(result, g)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CostTotal > result[j].CostTotal
	})
	return result
}

// top column widths
const (
	twPID   = 3
	twProj  = 14
	twAgent = 8
	twReqs  = 5
	twNum   = 9
	twHit   = 5
	twCost  = 9
)

func topHeader() string {
	return fmt.Sprintf(" %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s",
		pad("PID", twPID),
		pad("PROJECT", twProj),
		pad("TYPE", twAgent),
		rpad("REQS", twReqs),
		rpad("CTX", twNum),
		rpad("INPUT", twNum),
		rpad("OUTPUT", twNum),
		rpad("THINK", twNum),
		rpad("C.READ", twNum),
		rpad("C.WRITE", twNum),
		rpad("C.HIT", twHit),
		rpad("TOTAL", twNum),
		rpad("$INPUT", twCost),
		rpad("$OUTPUT", twCost),
		rpad("$CACHE", twCost),
		rpad("$SELF", twCost),
		rpad("$GROUP", twCost),
	)
}

func topMainRow(idx int, g sessionGroup) string {
	projStr := truncate(g.Project, twProj)
	if projStr == "" {
		projStr = "(unknown)"
	}
	projC := projectColor(g.Project)

	row := g.Main
	if row == nil && len(g.Subagents) > 0 {
		row = g.Subagents[0]
	}
	if row == nil {
		return ""
	}

	return topRowLine(
		pad(fmt.Sprintf("%d", idx), twPID),
		projC+pad(projStr, twProj)+reset,
		dim+pad("main", twAgent)+reset,
		row,
		g.CostTotal,
		g.Profile,
	)
}

func topSubagentRow(row *sessionRow, last bool, profile model.SessionProfile) string {
	tree := "├─"
	if last {
		tree = "└─"
	}
	prefix := dim + " " + tree + reset
	// Pad to match PID width (tree chars are 2 visible + 1 leading space = 3)
	return topRowLine(
		prefix,
		dim+pad("", twProj)+reset,
		cyan+pad("subagent", twAgent)+reset,
		row,
		-1,
		profile,
	)
}

func topRowLine(pidCol string, projCol string, typeCol string, row *sessionRow, groupCost float64, profile model.SessionProfile) string {
	totalInput := row.Input + row.CacheWrite + row.CacheRead
	cacheHitPct := "-"
	if totalInput > 0 {
		pct := float64(row.CacheRead) * 100 / float64(totalInput)
		cacheHitPct = fmt.Sprintf("%d%%", int(pct))
	}
	cacheCost := row.CostCRead + row.CostCWrite

	groupStr := rpad("", twCost)
	if groupCost >= 0 {
		groupStr = bold + white + rpad(model.FormatCost(groupCost), twCost) + reset
	}

	return fmt.Sprintf(" %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s",
		pidCol,
		projCol,
		typeCol,
		rpad(fmt.Sprintf("%d", row.RecordCount), twReqs),
		colorCtx(rpad(model.FormatTokens(row.CtxSize), twNum), row.CtxSize, profile),
		colorNum(rpad(model.FormatTokens(row.Input), twNum), row.Input),
		colorNum(rpad(model.FormatTokens(row.Output), twNum), row.Output),
		colorNum(rpad(model.FormatTokens(row.Thinking), twNum), row.Thinking),
		colorNum(rpad(model.FormatTokens(row.CacheRead), twNum), row.CacheRead),
		colorNum(rpad(model.FormatTokens(row.CacheWrite), twNum), row.CacheWrite),
		rpad(cacheHitPct, twHit),
		rpad(model.FormatTokens(row.TotalTokens), twNum),
		fmtCost(row.CostInput+row.CostThink, twCost),
		fmtCost(row.CostOutput, twCost),
		fmtCost(cacheCost, twCost),
		yellow+rpad(model.FormatCost(row.CostTotal), twCost)+reset,
		groupStr,
	)
}

func runTop(path string, cmd *TopCmd) {
	w := watcher.New(path, false, false)

	state := newTopState(cmd.Window)

	files, err := parser.FindJSONLFiles(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: error scanning files: %v\n", err)
	}
	for _, f := range files {
		recs, fErr := parser.ParseFile(f)
		if fErr != nil {
			continue
		}
		state.seed(recs, cmd.Window)
	}

	if err := w.SeekToEnd(); err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning files: %v\n", err)
		os.Exit(1)
	}

	w.Start()
	defer w.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	fmt.Print("\033[?25l") // hide cursor once

	renderTop(state, cmd)

	for {
		select {
		case <-sig:
			fmt.Print("\033[?25h")
			return
		case recs, ok := <-w.Records():
			if !ok {
				fmt.Print("\033[?25h")
				return
			}
			for _, r := range recs {
				if r.Role == "assistant" {
					state.add(r)
				}
			}
			renderTop(state, cmd)
		case <-ticker.C:
			state.mu.Lock()
			dirty := state.dirty
			state.mu.Unlock()
			if dirty {
				renderTop(state, cmd)
			}
		}
	}
}

func renderTop(state *topState, cmd *TopCmd) {
	now := time.Now()
	groups := state.snapshot(now, cmd.Expand)

	state.mu.Lock()
	allTimeCost := state.allTime.TotalCost
	allTimeTokens := state.allTime.TotalTokens
	sessions := state.allTime.SessionCount
	state.mu.Unlock()

	height := termHeight()

	fmt.Print("\033[H\033[2J")

	windowLabel := formatDuration(cmd.Window)
	fmt.Printf("%s%stop - Claude Code Token Monitor%s  %s│%s  window: %s%s%s  %s│%s  %s  %s│%s  sessions: %d  %s│%s  %s\n",
		bold, white, reset,
		dim, reset,
		bold, windowLabel, reset,
		dim, reset,
		now.Format("15:04:05"),
		dim, reset,
		sessions,
		dim, reset,
		dim+"all-time: "+model.FormatTokens(allTimeTokens)+" "+yellow+model.FormatCost(allTimeCost)+reset,
	)

	var windowCost float64
	var windowTokens int
	for _, g := range groups {
		if g.Main != nil {
			windowTokens += g.Main.TotalTokens
		}
		for _, s := range g.Subagents {
			windowTokens += s.TotalTokens
		}
		windowCost += g.CostTotal
	}

	fmt.Printf("%sSessions: %d%s  %s│%s  window tokens: %s  %s│%s  window cost: %s%s%s\n",
		dim, len(groups), reset,
		dim, reset,
		model.FormatTokens(windowTokens),
		dim, reset,
		yellow+bold, model.FormatCost(windowCost), reset,
	)

	fmt.Println()

	fmt.Printf("%s%s%s%s\n", bold, cyan, topHeader(), reset)

	maxLines := height - 5
	if maxLines < 1 {
		maxLines = 1
	}
	lines := 0
	for i, g := range groups {
		if lines >= maxLines {
			break
		}

		// Main row (promote first subagent if no main)
		if g.Main != nil {
			fmt.Println(topMainRow(i+1, g))
			lines++
		} else if len(g.Subagents) > 0 {
			fmt.Println(topMainRow(i+1, g))
			lines++
			g.Subagents = g.Subagents[1:]
		}

		// Subagent rows, indented
		for j, s := range g.Subagents {
			if lines >= maxLines {
				break
			}
			fmt.Println(topSubagentRow(s, j == len(g.Subagents)-1, g.Profile))
			lines++
		}
	}
}

// colorCtx highlights context size using the session's profile-based
// thresholds (shared with the tail mode's compact-hint system).
func colorCtx(s string, n int, profile model.SessionProfile) string {
	if n == 0 {
		return dim + s + reset
	}
	if n >= model.ContextWarningThreshold(profile) {
		return red + bold + s + reset
	}
	if n >= model.ContextThreshold(profile) {
		return yellow + s + reset
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
