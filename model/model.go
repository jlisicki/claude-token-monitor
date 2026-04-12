package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type TokenRecord struct {
	Role                string // "assistant", "user", "system", etc.
	Model               string
	InputTokens         int
	OutputTokens        int
	ThinkingTokens      int
	CacheCreationTokens int
	CacheWrite5m        int
	CacheWrite1h        int
	CacheReadTokens     int
	Timestamp           time.Time
	SessionID           string
	AgentID             string
	Project             string
	ContentType         string
	ContentSize         int
	RawLine             string
	MessageID           string // API message ID (msg_...), shared across content blocks from one response
	StopReason          string // "end_turn", "tool_use", or "" for intermediate streaming entries
}

type ModelPricing struct {
	InputPerMillion       float64
	OutputPerMillion      float64
	CacheWritePerMillion  float64
	CacheReadPerMillion   float64
}

var PricingTable = map[string]ModelPricing{
	"opus": {
		InputPerMillion:      15.0,
		OutputPerMillion:     75.0,
		CacheWritePerMillion: 18.75,
		CacheReadPerMillion:  1.50,
	},
	"sonnet": {
		InputPerMillion:      3.0,
		OutputPerMillion:     15.0,
		CacheWritePerMillion: 3.75,
		CacheReadPerMillion:  0.30,
	},
	"haiku": {
		InputPerMillion:      0.80,
		OutputPerMillion:     4.0,
		CacheWritePerMillion: 1.0,
		CacheReadPerMillion:  0.08,
	},
}

func NormalizeModelName(model string) string {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "opus") {
		return "opus"
	}
	if strings.Contains(lower, "sonnet") {
		return "sonnet"
	}
	if strings.Contains(lower, "haiku") {
		return "haiku"
	}
	return model
}

type RecordCosts struct {
	Output     float64
	Thinking   float64
	CacheRead  float64
	CacheWrite float64
	Input      float64
	Total      float64
}

// PricingFor returns the pricing for a model name (raw or normalized), falling back to sonnet.
func PricingFor(modelName string) ModelPricing {
	norm := NormalizeModelName(modelName)
	if p, ok := PricingTable[norm]; ok {
		return p
	}
	return PricingTable["sonnet"]
}

func CostsForRecord(r TokenRecord) RecordCosts {
	pricing := PricingFor(r.Model)
	c := RecordCosts{
		Input:      float64(r.InputTokens) * pricing.InputPerMillion / 1_000_000,
		Output:     float64(r.OutputTokens) * pricing.OutputPerMillion / 1_000_000,
		Thinking:   float64(r.ThinkingTokens) * pricing.OutputPerMillion / 1_000_000,
		CacheWrite: float64(r.CacheCreationTokens) * pricing.CacheWritePerMillion / 1_000_000,
		CacheRead:  float64(r.CacheReadTokens) * pricing.CacheReadPerMillion / 1_000_000,
	}
	c.Total = c.Input + c.Output + c.Thinking + c.CacheWrite + c.CacheRead
	return c
}

func CostForRecord(r TokenRecord) float64 {
	return CostsForRecord(r).Total
}

type ModelSummary struct {
	Model               string
	InputTokens         int
	OutputTokens        int
	ThinkingTokens      int
	CacheCreationTokens int
	CacheReadTokens     int
	TotalTokens         int
	Cost                float64
}

type WindowSummary struct {
	Label       string
	TotalTokens int
	TotalCost   float64
}

type Summary struct {
	InputTokens         int
	OutputTokens        int
	ThinkingTokens      int
	CacheCreationTokens int
	CacheReadTokens     int
	TotalTokens         int
	TotalCost           float64
	ByModel             map[string]*ModelSummary
	SessionCount        int
	LastUpdate          time.Time
	Records             []TokenRecord
	KeepRecords         bool // set to true to retain records for windowed stats
	sessions            map[string]bool
}

func NewSummary() *Summary {
	return &Summary{
		ByModel:  make(map[string]*ModelSummary),
		sessions: make(map[string]bool),
	}
}

func (s *Summary) Add(r TokenRecord) {
	if r.Role != "assistant" {
		return
	}
	if s.KeepRecords {
		s.Records = append(s.Records, r)
	}
	if r.SessionID != "" && !s.sessions[r.SessionID] {
		s.sessions[r.SessionID] = true
		s.SessionCount++
	}
	s.InputTokens += r.InputTokens
	s.OutputTokens += r.OutputTokens
	s.ThinkingTokens += r.ThinkingTokens
	s.CacheCreationTokens += r.CacheCreationTokens
	s.CacheReadTokens += r.CacheReadTokens
	total := r.TotalTokens()
	s.TotalTokens += total

	cost := CostForRecord(r)
	s.TotalCost += cost
	s.LastUpdate = time.Now()

	norm := NormalizeModelName(r.Model)
	ms, ok := s.ByModel[norm]
	if !ok {
		ms = &ModelSummary{Model: norm}
		s.ByModel[norm] = ms
	}
	ms.InputTokens += r.InputTokens
	ms.OutputTokens += r.OutputTokens
	ms.ThinkingTokens += r.ThinkingTokens
	ms.CacheCreationTokens += r.CacheCreationTokens
	ms.CacheReadTokens += r.CacheReadTokens
	ms.TotalTokens += total
	ms.Cost += cost
}

const maxWindowDuration = 60 * time.Minute

func (s *Summary) pruneOldRecords(now time.Time) {
	cutoff := now.Add(-maxWindowDuration)
	i := sort.Search(len(s.Records), func(i int) bool {
		return !s.Records[i].Timestamp.Before(cutoff)
	})
	if i > 0 {
		s.Records = s.Records[i:]
	}
}

func (s *Summary) WindowSummaries(now time.Time) []WindowSummary {
	s.pruneOldRecords(now)

	windows := []struct {
		label    string
		duration time.Duration
	}{
		{"Last 5m", 5 * time.Minute},
		{"Last 15m", 15 * time.Minute},
		{"Last 60m", 60 * time.Minute},
	}

	results := make([]WindowSummary, len(windows))
	for i, w := range windows {
		cutoff := now.Add(-w.duration)
		var tokens int
		var cost float64
		for _, r := range s.Records {
			if !r.Timestamp.Before(cutoff) {
				tokens += r.TotalTokens()
				cost += CostForRecord(r)
			}
		}
		results[i] = WindowSummary{
			Label:       w.label,
			TotalTokens: tokens,
			TotalCost:   cost,
		}
	}
	return results
}

// TotalInputTokens returns all input tokens (uncached + cache write + cache read).
func (s *Summary) TotalInputTokens() int {
	return s.InputTokens + s.CacheCreationTokens + s.CacheReadTokens
}

// TotalInputCost returns the combined cost of all input tokens.
func (s *Summary) TotalInputCost() float64 {
	return s.CostByTokenType("input") + s.CostByTokenType("cache_write") + s.CostByTokenType("cache_read")
}

func (s *Summary) CostByTokenType(tokenType string) float64 {
	var total float64
	for name, ms := range s.ByModel {
		pricing := PricingFor(name)
		var tokens int
		var rate float64
		switch tokenType {
		case "input":
			tokens = ms.InputTokens
			rate = pricing.InputPerMillion
		case "output":
			tokens = ms.OutputTokens
			rate = pricing.OutputPerMillion
		case "thinking":
			tokens = ms.ThinkingTokens
			rate = pricing.OutputPerMillion
		case "cache_write":
			tokens = ms.CacheCreationTokens
			rate = pricing.CacheWritePerMillion
		case "cache_read":
			tokens = ms.CacheReadTokens
			rate = pricing.CacheReadPerMillion
		}
		total += float64(tokens) * rate / 1_000_000
	}
	return total
}

func (r TokenRecord) TotalInputTokens() int {
	return r.InputTokens + r.CacheCreationTokens + r.CacheReadTokens
}

func (r TokenRecord) TotalTokens() int {
	return r.InputTokens + r.OutputTokens + r.ThinkingTokens + r.CacheCreationTokens + r.CacheReadTokens
}

func SortedModels(m map[string]*ModelSummary) []*ModelSummary {
	models := make([]*ModelSummary, 0, len(m))
	for _, ms := range m {
		models = append(models, ms)
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].Cost > models[j].Cost
	})
	return models
}

func DisplayName(name string) string {
	switch name {
	case "opus":
		return "Opus"
	case "sonnet":
		return "Sonnet"
	case "haiku":
		return "Haiku"
	default:
		return name
	}
}

func FormatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

func FormatCost(c float64) string {
	return fmt.Sprintf("$%.2f", c)
}
