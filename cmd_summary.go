package main

import (
	"fmt"
	"os"
	"time"
	"github.com/jlisicki/claude-token-monitor/model"
	"github.com/jlisicki/claude-token-monitor/parser"
)

func runSummary(path string) {
	files, err := parser.FindJSONLFiles(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	summary := model.NewSummary()
	summary.KeepRecords = true // needed for WindowSummaries

	for _, f := range files {
		recs, err := parser.ParseFile(f)
		if err != nil {
			continue
		}
		for _, r := range recs {
			summary.Add(r)
		}
	}

	fmt.Println("Claude Code Token Summary")
	fmt.Println("═════════════════════════════════════════════")
	fmt.Printf("  %-16s %14s %12s\n", "Type", "Tokens", "Cost")
	fmt.Println("  ────────────────────────────────────────────")
	fmt.Printf("  %-16s %14s %12s\n", "Input", model.FormatTokens(summary.TotalInputTokens()), model.FormatCost(summary.TotalInputCost()))
	fmt.Printf("    %-14s %14s %12s\n", "Cache read", model.FormatTokens(summary.CacheReadTokens), model.FormatCost(summary.CostByTokenType("cache_read")))
	fmt.Printf("    %-14s %14s %12s\n", "Cache write", model.FormatTokens(summary.CacheCreationTokens), model.FormatCost(summary.CostByTokenType("cache_write")))
	fmt.Printf("    %-14s %14s %12s\n", "Uncached", model.FormatTokens(summary.InputTokens), model.FormatCost(summary.CostByTokenType("input")))
	fmt.Printf("  %-16s %14s %12s\n", "Output", model.FormatTokens(summary.OutputTokens), model.FormatCost(summary.CostByTokenType("output")))
	fmt.Printf("  %-16s %14s %12s\n", "Thinking", model.FormatTokens(summary.ThinkingTokens), model.FormatCost(summary.CostByTokenType("thinking")))
	fmt.Println("  ────────────────────────────────────────────")
	fmt.Printf("  %-16s %14s %12s\n", "Total", model.FormatTokens(summary.TotalTokens), model.FormatCost(summary.TotalCost))
	fmt.Println()
	fmt.Println("  By Model:")
	models := model.SortedModels(summary.ByModel)
	for _, ms := range models {
		fmt.Printf("    %-14s %14s %12s\n", model.DisplayName(ms.Model), model.FormatTokens(ms.TotalTokens), model.FormatCost(ms.Cost))
	}
	fmt.Println()
	fmt.Println("  Recent Activity:")
	windows := summary.WindowSummaries(time.Now())
	for _, w := range windows {
		fmt.Printf("    %-14s %14s %12s\n", w.Label, model.FormatTokens(w.TotalTokens), model.FormatCost(w.TotalCost))
	}
	fmt.Println()
	fmt.Printf("  Sessions: %d  │  Files: %d\n", summary.SessionCount, len(files))
	fmt.Printf("  Path: %s\n", path)
}

