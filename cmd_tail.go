package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"github.com/jlisicki/claude-token-monitor/model"
	"github.com/jlisicki/claude-token-monitor/watcher"

	"golang.org/x/term"
)

const (
	reset   = "\033[0m"
	dim     = "\033[2m"
	bold    = "\033[1m"
	cyan    = "\033[36m"
	yellow  = "\033[33m"
	white   = "\033[37m"
	red     = "\033[31m"
	green   = "\033[32m"
	blue    = "\033[34m"
	magenta = "\033[35m"
)

func termHeight() int {
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// col widths
const (
	wTime  = 8
	wModel = 7
	wAgent = 3
	wType  = 8
	wProj  = 12
	wNum   = 8
	wCW    = 16 // cache write with TTL
	wHit   = 5
	wTotal = 10
)

// prefix width = marker + TIME + PROJECT + MODEL + AGENT + TYPE + gaps
var prefixWidth = 1 + wTime + 1 + wProj + 1 + wModel + 1 + wAgent + 1 + wType

// data width = INPUT + OUTPUT + THINK + C.READ + C.WRITE + C.HIT + TOTAL + gaps
var lineWidth = prefixWidth + wNum*4 + 4 + wCW + 1 + wHit + 1 + wTotal

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func rpad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// projectColors assigns a stable color to each project name via hash.
var projectColorCodes = []string{
	"\033[38;5;114m", // light green
	"\033[38;5;180m", // tan
	"\033[38;5;146m", // light purple
	"\033[38;5;174m", // pink
	"\033[38;5;109m", // teal
	"\033[38;5;223m", // peach
	"\033[38;5;152m", // light cyan
	"\033[38;5;217m", // salmon
}

func projectColor(name string) string {
	if name == "" {
		return dim
	}
	var h uint32
	for _, c := range name {
		h = h*31 + uint32(c)
	}
	return projectColorCodes[h%uint32(len(projectColorCodes))]
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04:05")
}

// sessionProfile is an alias for the shared model type.
type sessionProfile = model.SessionProfile

const (
	profileExploring  = model.ProfileExploring
	profileCoding     = model.ProfileCoding
	profileReasoning  = model.ProfileReasoning
	profileDelegating = model.ProfileDelegating
)

// sessionStats tracks observable signals for a single session.
type sessionStats struct {
	records        int
	toolUseRecords int // records with tool_use content type
	thinkingTokens int
	outputTokens   int
	subagentCalls  int
	cacheReads     []int // sliding window of recent cache_read values
	cacheWriteSum  int
	lastModel      string
}

const statsWindow = 5 // sliding window for cache reads

func (ss *sessionStats) addRecord(r model.TokenRecord) {
	ss.records++
	if strings.Contains(r.ContentType, "tool_use") {
		ss.toolUseRecords++
	}
	ss.thinkingTokens += r.ThinkingTokens
	ss.outputTokens += r.OutputTokens + r.ThinkingTokens
	if r.AgentID != "" {
		ss.subagentCalls++
	}
	ss.cacheWriteSum += r.CacheCreationTokens
	ss.lastModel = r.Model

	ss.cacheReads = append(ss.cacheReads, r.CacheReadTokens)
	if len(ss.cacheReads) > statsWindow {
		ss.cacheReads = ss.cacheReads[len(ss.cacheReads)-statsWindow:]
	}
}

func (ss *sessionStats) avgCacheRead() int {
	if len(ss.cacheReads) == 0 {
		return 0
	}
	sum := 0
	for _, v := range ss.cacheReads {
		sum += v
	}
	return sum / len(ss.cacheReads)
}

func (ss *sessionStats) detectProfile() sessionProfile {
	return model.ClassifyProfile(ss.records, ss.toolUseRecords, ss.subagentCalls, ss.thinkingTokens, ss.outputTokens)
}

// profileThreshold delegates to the shared model.ContextThreshold.
func profileThreshold(p sessionProfile) int {
	return model.ContextThreshold(p)
}

// compactHint tracks per-session context growth to suggest compaction.
type compactHint struct {
	sessions map[string]*sessionStats
	hinted   map[string]bool
}

func newCompactHint() *compactHint {
	return &compactHint{
		sessions: make(map[string]*sessionStats),
		hinted:   make(map[string]bool),
	}
}

const compactWindowSize = 3 // consecutive calls above threshold before hinting

func (ch *compactHint) getStats(sid string) *sessionStats {
	ss, ok := ch.sessions[sid]
	if !ok {
		ss = &sessionStats{}
		ch.sessions[sid] = ss
	}
	return ss
}

// check returns a hint message if the session should compact, or empty string.
func (ch *compactHint) check(r model.TokenRecord) string {
	sid := r.SessionID
	if sid == "" || ch.hinted[sid] {
		return ""
	}

	ss := ch.getStats(sid)
	ss.addRecord(r)

	if len(ss.cacheReads) < compactWindowSize {
		return ""
	}

	profile := ss.detectProfile()
	threshold := profileThreshold(profile)

	// Check if recent calls consistently exceed the profile-specific threshold
	window := ss.cacheReads[len(ss.cacheReads)-compactWindowSize:]
	for _, cr := range window {
		if cr < threshold {
			return ""
		}
	}

	ch.hinted[sid] = true

	avgRead := ss.avgCacheRead()
	pricing := model.PricingFor(r.Model)
	costPerCall := float64(avgRead) * pricing.CacheReadPerMillion / 1_000_000

	var reason string
	switch profile {
	case profileExploring:
		reason = "context is mostly stale tool results"
	case profileDelegating:
		reason = "subagents carry their own context"
	case profileCoding:
		reason = "context growing with diminishing returns"
	case profileReasoning:
		reason = "even for reasoning, context is getting expensive"
	default:
		reason = "context is large"
	}

	return fmt.Sprintf("%s (%s session, ~%s tokens, %s/call) — consider /compact",
		reason, profile, model.FormatTokens(avgRead), model.FormatCost(costPerCall))
}

// reset clears the hint for a session (e.g., after compaction detected via drop in cache reads).
func (ch *compactHint) reset(sid string) {
	delete(ch.hinted, sid)
	delete(ch.sessions, sid)
}

// detectCompaction checks if cache reads dropped sharply, indicating a compaction happened.
func (ch *compactHint) detectCompaction(r model.TokenRecord) {
	ss, ok := ch.sessions[r.SessionID]
	if !ok || len(ss.cacheReads) == 0 {
		return
	}
	lastRead := ss.cacheReads[len(ss.cacheReads)-1]
	threshold := profileThreshold(ss.detectProfile())
	if lastRead > threshold && r.CacheReadTokens < lastRead/3 {
		ch.reset(r.SessionID)
	}
}

// streamingState tracks the in-flight message being displayed so it can be
// updated in-place as new content blocks stream in.
type streamingState struct {
	key          string   // agentID:messageID
	linesPrinted int      // terminal lines occupied by current display
	contentTypes []string // accumulated content types from intermediates
}

// eraseLines moves the cursor up n lines and clears them.
func eraseLines(n int) {
	for i := 0; i < n; i++ {
		fmt.Print("\033[A\033[2K")
	}
}

func runTail(path string, debug bool, verbose bool, history bool) {
	w := watcher.New(path, verbose, debug)
	w.SetStreamAll(true) // receive intermediates for live streaming display

	var summary *model.Summary
	if history {
		var err error
		summary, err = w.InitialScan()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning files: %v\n", err)
			os.Exit(1)
		}
	} else {
		summary = model.NewSummary()
		if err := w.SeekToEnd(); err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning files: %v\n", err)
			os.Exit(1)
		}
	}

	session := model.NewSummary()
	lineCount := 0
	linesSinceHeader := 0
	var costSum float64
	hints := newCompactHint()
	var streaming *streamingState

	if history {
		printHeaderWithSummary("history", summary)
	} else {
		printHeaderWithSummary("session", session)
	}

	w.Start()
	defer w.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	quit := make(chan struct{})
	if restore, err := enableKeypress(int(os.Stdin.Fd())); err == nil {
		defer restore()
		go func() {
			buf := make([]byte, 1)
			for {
				if _, err := os.Stdin.Read(buf); err != nil {
					return
				}
				if buf[0] == 'q' || buf[0] == 'Q' {
					close(quit)
					return
				}
			}
		}()
	}

	for {
		select {
		case <-sig:
			fmt.Println()
			printFinalSummary(summary, session)
			return
		case <-quit:
			fmt.Println()
			printFinalSummary(summary, session)
			return
		case recs, ok := <-w.Records():
			if !ok {
				return
			}
			for _, r := range recs {
				if r.Role != "assistant" {
					// Non-assistant record breaks any in-place streaming.
					streaming = nil
					printVerboseRecord(r)
					linesSinceHeader++
					if debug {
						printDebug(r)
						linesSinceHeader++
					}
					continue
				}

				key := r.AgentID + ":" + r.MessageID
				isFinal := r.StopReason != ""

				if r.MessageID != "" && !isFinal {
					// Intermediate streaming entry.
					if streaming != nil && streaming.key == key {
						// Update in place — erase previous lines and reprint.
						eraseLines(streaming.linesPrinted)
						linesSinceHeader -= streaming.linesPrinted
					} else {
						// New streaming message (or different message).
						streaming = &streamingState{key: key}
					}
					streaming.contentTypes = appendContentType(streaming.contentTypes, r.ContentType)
					r.ContentType = mergeContentTypes(streaming.contentTypes)
					lines := printStreamingRecord(r)
					streaming.linesPrinted = lines
					linesSinceHeader += lines
					continue
				}

				// Final entry (or record without MessageID).
				if streaming != nil && streaming.key == key {
					// Replace streaming display with final version.
					eraseLines(streaming.linesPrinted)
					linesSinceHeader -= streaming.linesPrinted
					// Merge content types from intermediates.
					all := appendContentType(streaming.contentTypes, r.ContentType)
					r.ContentType = mergeContentTypes(all)
				}
				streaming = nil

				summary.Add(r)
				session.Add(r)
				lineCount++
				costs := model.CostsForRecord(r)
				costSum += costs.Total
				avgCost := costSum / float64(lineCount)

				// Detect compaction (cache reads drop significantly)
				hints.detectCompaction(r)

				// Each record takes 2-3 lines; header takes 3 lines
				if linesSinceHeader >= termHeight()-6 {
					printHeaderWithSummary("session", session)
					linesSinceHeader = 0
				}
				expensive := lineCount > 2 && costs.Total > avgCost*3
				lines := printRecordWithCosts(r, costs, expensive)
				linesSinceHeader += lines

				// Check for compaction hint
				if hint := hints.check(r); hint != "" {
					fmt.Printf("  %s💡 %s%s\n", yellow, hint, reset)
					linesSinceHeader++
				}

				if debug {
					printDebug(r)
					linesSinceHeader++
				}
			}
		}
	}
}

// compactContentType condenses a merged content type to fit the 8-char TYPE
// column.  Thinking is dropped when other types are present (it's already
// visible in the THINK token column).
func compactContentType(ct string) string {
	switch ct {
	case "thinking+text+tool_use", "text+tool_use":
		return "txt+tool"
	case "thinking+text":
		return "text"
	case "thinking+tool_use":
		return "tool_use"
	default:
		return ct
	}
}

func appendContentType(types []string, ct string) []string {
	if ct == "" {
		return types
	}
	for _, t := range types {
		if t == ct {
			return types
		}
	}
	return append(types, ct)
}

// mergeContentTypes joins content type strings, deduplicating.
func mergeContentTypes(types []string) string {
	result := ""
	for i, t := range types {
		if i > 0 {
			result += "+"
		}
		result += t
	}
	return result
}

// printStreamingRecord prints a record with a streaming indicator.
// Returns the number of terminal lines printed.
func printStreamingRecord(r model.TokenRecord) int {
	ts := formatTime(r.Timestamp)
	norm := model.NormalizeModelName(r.Model)

	modelColor := white
	switch norm {
	case "opus":
		modelColor = magenta
	case "sonnet":
		modelColor = blue
	case "haiku":
		modelColor = green
	}

	proj := truncate(r.Project, wProj)
	projC := projectColor(r.Project)
	ct := truncate(compactContentType(r.ContentType), wType)

	agentStr := "main"
	agentColor := dim
	if r.AgentID != "" {
		agentStr = "sub"
		agentColor = cyan
	}

	marker := yellow + "⟳" + reset

	fmt.Printf("%s%s %s%s%s %s%s%s %s %s %s%s…%s\n",
		marker,
		pad(ts, wTime),
		projC, pad(proj, wProj), reset,
		modelColor, pad(norm, wModel), reset,
		agentColor+pad(agentStr, wAgent)+reset,
		pad(ct, wType),
		dim, "streaming", reset,
	)
	return 1
}

func printHeaderWithSummary(label string, s *model.Summary) {
	info := fmt.Sprintf(" %s: %s tokens  %s ", label, model.FormatTokens(s.TotalTokens), model.FormatCost(s.TotalCost))
	dashLeft := (lineWidth - len(info)) / 2
	if dashLeft < 2 {
		dashLeft = 2
	}
	dashRight := lineWidth - dashLeft - len(info)
	if dashRight < 2 {
		dashRight = 2
	}
	fmt.Printf("%s%s%s%s%s%s%s\n",
		dim, strings.Repeat("─", dashLeft),
		reset+bold+yellow, info,
		reset+dim, strings.Repeat("─", dashRight), reset,
	)

	prefix := " " + pad("TIME", wTime) + " " + pad("PROJECT", wProj) + " " + pad("MODEL", wModel) + " " + pad("AGT", wAgent) + " " + pad("TYPE", wType)
	data := rpad("INPUT", wNum) + " " + rpad("OUTPUT", wNum) + " " + rpad("THINK", wNum) + " " + rpad("C.READ", wNum) + " " + rpad("C.WRITE", wCW) + " " + rpad("C.HIT", wHit) + " " + rpad("TOTAL", wTotal)
	fmt.Printf("%s%s%s %s%s\n", bold, cyan, prefix, data, reset)
}

func printRecordWithCosts(r model.TokenRecord, costs model.RecordCosts, expensive bool) int {
	ts := formatTime(r.Timestamp)
	norm := model.NormalizeModelName(r.Model)

	modelColor := white
	switch norm {
	case "opus":
		modelColor = magenta
	case "sonnet":
		modelColor = blue
	case "haiku":
		modelColor = green
	}

	proj := truncate(r.Project, wProj)
	projC := projectColor(r.Project)
	ct := truncate(compactContentType(r.ContentType), wType)

	agentStr := "main"
	agentColor := dim
	if r.AgentID != "" {
		agentStr = "sub"
		agentColor = cyan
	}

	totalInput := r.TotalInputTokens()
	cacheHitPct := "-"
	if totalInput > 0 {
		pct := float64(r.CacheReadTokens) * 100 / float64(totalInput)
		cacheHitPct = fmt.Sprintf("%d%%", int(pct))
	}

	cacheWriteStr := fmtCacheWrite(r)
	totalTokens := r.TotalTokens()

	// Highlight marker for expensive lines
	marker := " "
	totalColor := bold
	if expensive {
		marker = red + "▶" + reset
		totalColor = red + bold
	}
	// Row 1: metadata + token counts + total tokens
	fmt.Printf("%s%s %s%s%s %s%s%s %s %s %s %s %s %s %s %s %s\n",
		marker,
		pad(ts, wTime),
		projC, pad(proj, wProj), reset,
		modelColor, pad(norm, wModel), reset,
		agentColor+pad(agentStr, wAgent)+reset,
		pad(ct, wType),
		colorNum(rpad(model.FormatTokens(r.InputTokens), wNum), r.InputTokens),
		colorNum(rpad(model.FormatTokens(r.OutputTokens), wNum), r.OutputTokens),
		colorNum(rpad(model.FormatTokens(r.ThinkingTokens), wNum), r.ThinkingTokens),
		colorNum(rpad(model.FormatTokens(r.CacheReadTokens), wNum), r.CacheReadTokens),
		cacheWriteStr,
		rpad(cacheHitPct, wHit),
		totalColor+rpad(model.FormatTokens(totalTokens), wTotal)+reset,
	)

	// Row 2: blank prefix + costs aligned under each column + total cost
	totalCostColor := yellow + bold
	if expensive {
		totalCostColor = red + bold
	}
	blank := strings.Repeat(" ", prefixWidth)
	fmt.Printf("%s%s %s %s %s %s %s %s %s%s\n",
		dim, blank,
		fmtCost(costs.Input, wNum),
		fmtCost(costs.Output, wNum),
		fmtCost(costs.Thinking, wNum),
		fmtCost(costs.CacheRead, wNum),
		fmtCost(costs.CacheWrite, wCW),
		rpad("", wHit),
		totalCostColor+rpad(model.FormatCost(costs.Total), wTotal)+reset,
		reset,
	)

	lines := 2
	if expensive {
		explanation := explainExpensive(r, costs)
		fmt.Printf(" %s%s⚑ %s%s\n", " ", red, explanation, reset)
		lines = 3
	}
	return lines
}

func explainExpensive(r model.TokenRecord, costs model.RecordCosts) string {
	norm := model.NormalizeModelName(r.Model)

	// Find the dominant cost category
	type reason struct {
		cost float64
		desc string
	}
	reasons := []reason{
		{costs.Output, fmt.Sprintf("output %s tokens", model.FormatTokens(r.OutputTokens))},
		{costs.Thinking, fmt.Sprintf("thinking %s tokens", model.FormatTokens(r.ThinkingTokens))},
		{costs.CacheWrite, fmt.Sprintf("cache write %s tokens", model.FormatTokens(r.CacheCreationTokens))},
		{costs.CacheRead, fmt.Sprintf("cache read %s tokens", model.FormatTokens(r.CacheReadTokens))},
		{costs.Input, fmt.Sprintf("uncached input %s tokens", model.FormatTokens(r.InputTokens))},
	}

	// Find top reason
	var top reason
	for _, r := range reasons {
		if r.cost > top.cost {
			top = r
		}
	}

	parts := []string{top.desc + " (" + model.FormatCost(top.cost) + ")"}

	// Add model note if it's opus (5x more expensive than sonnet)
	if norm == "opus" {
		parts = append(parts, "opus pricing")
	}

	// Add cache miss note
	totalInput := r.TotalInputTokens()
	if totalInput > 0 {
		hitRate := float64(r.CacheReadTokens) * 100 / float64(totalInput)
		if hitRate < 50 {
			parts = append(parts, fmt.Sprintf("low cache hit %d%%", int(hitRate)))
		}
	}

	return strings.Join(parts, ", ")
}

func fmtCost(c float64, width int) string {
	s := model.FormatCost(c)
	padded := rpad(s, width)
	if c < 0.005 {
		return dim + padded + reset
	}
	return yellow + padded + reset
}


func printVerboseRecord(r model.TokenRecord) {
	ts := formatTime(r.Timestamp)
	proj := truncate(r.Project, wProj)
	ct := truncate(r.ContentType, wType)

	sizeStr := ""
	if r.ContentSize > 0 {
		sizeStr = model.FormatTokens(r.ContentSize) + "b"
	}

	role := r.Role
	roleColor := dim
	if role == "user" {
		roleColor = yellow
	} else if role == "system" {
		roleColor = cyan
	}

	agentStr := "   "
	if r.AgentID != "" {
		agentStr = "sub"
	}

	projC := projectColor(r.Project)
	fmt.Printf(" %s%s %s%s%s %s%-7s%s %s %s %s%s\n",
		dim,
		pad(ts, wTime),
		projC, pad(proj, wProj), reset+dim,
		roleColor, pad(role, wModel), reset+dim,
		pad(agentStr, wAgent),
		pad(ct, wType),
		sizeStr,
		reset,
	)
}

// fmtCacheWrite returns a colored, pre-padded cache write string.
// Colors: red for @5m (short-lived), cyan for @1h (longer).
func fmtCacheWrite(r model.TokenRecord) string {
	if r.CacheCreationTokens == 0 {
		return dim + rpad("0", wCW) + reset
	}
	var plain string
	var colored string
	if r.CacheWrite5m > 0 && r.CacheWrite1h > 0 {
		s5 := model.FormatTokens(r.CacheWrite5m)
		s1 := model.FormatTokens(r.CacheWrite1h)
		plain = s5 + "@5m " + s1 + "@1h"
		colored = s5 + red + "@5m " + reset + s1 + cyan + "@1h" + reset
	} else if r.CacheWrite5m > 0 {
		s := model.FormatTokens(r.CacheWrite5m)
		plain = s + "@5m"
		colored = s + red + "@5m" + reset
	} else if r.CacheWrite1h > 0 {
		s := model.FormatTokens(r.CacheWrite1h)
		plain = s + "@1h"
		colored = s + cyan + "@1h" + reset
	} else {
		s := model.FormatTokens(r.CacheCreationTokens)
		return rpad(s, wCW)
	}
	padding := wCW - len(plain)
	if padding > 0 {
		return strings.Repeat(" ", padding) + colored
	}
	return colored
}

func colorNum(s string, n int) string {
	if n == 0 {
		return dim + s + reset
	}
	return s
}

func printDebug(r model.TokenRecord) {
	raw := r.RawLine
	if len(raw) > 1024 {
		raw = raw[:1024] + "..."
	}
	fmt.Printf("  %s[debug] %s%s\n", dim, raw, reset)
}

func printFinalSummary(total *model.Summary, session *model.Summary) {
	fmt.Println(dim + strings.Repeat("─", lineWidth) + reset)
	fmt.Printf("%s%sSession Summary%s\n", bold, cyan, reset)
	fmt.Printf("  %-14s %10s  %s%s%s\n", "Input", model.FormatTokens(session.InputTokens), yellow, model.FormatCost(session.CostByTokenType("input")), reset)
	fmt.Printf("  %-14s %10s  %s%s%s\n", "Output", model.FormatTokens(session.OutputTokens), yellow, model.FormatCost(session.CostByTokenType("output")), reset)
	fmt.Printf("  %-14s %10s  %s%s%s\n", "Thinking", model.FormatTokens(session.ThinkingTokens), yellow, model.FormatCost(session.CostByTokenType("thinking")), reset)
	fmt.Printf("  %-14s %10s  %s%s%s\n", "Cache read", model.FormatTokens(session.CacheReadTokens), yellow, model.FormatCost(session.CostByTokenType("cache_read")), reset)
	fmt.Printf("  %-14s %10s  %s%s%s\n", "Cache write", model.FormatTokens(session.CacheCreationTokens), yellow, model.FormatCost(session.CostByTokenType("cache_write")), reset)
	fmt.Println("  ────────────────────────────────────")
	fmt.Printf("  %s%-14s %10s  %s%s%s%s\n", bold, "Total", model.FormatTokens(session.TotalTokens), yellow, model.FormatCost(session.TotalCost), reset, reset)
	fmt.Println()
	fmt.Printf("  %s%-14s %10s  %s%s%s%s\n", dim, "All-time", model.FormatTokens(total.TotalTokens), yellow, model.FormatCost(total.TotalCost), reset, reset)
}
