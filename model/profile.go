package model

import "strings"

// SessionProfile classifies how a session is being used.
type SessionProfile int

const (
	ProfileUnknown    SessionProfile = iota
	ProfileExploring  // heavy tool_use, reading files, searching — context fills with stale tool results
	ProfileCoding     // mixed tool_use and text output — moderate context value
	ProfileReasoning  // heavy thinking, architectural discussion — context is high-value
	ProfileDelegating // heavy subagent usage — main context less critical
)

func (p SessionProfile) String() string {
	switch p {
	case ProfileExploring:
		return "exploring"
	case ProfileCoding:
		return "coding"
	case ProfileReasoning:
		return "reasoning"
	case ProfileDelegating:
		return "delegating"
	default:
		return "unknown"
	}
}

// ContextThreshold returns the context-size threshold (in tokens) at which
// a session of the given profile is considered "large".
func ContextThreshold(p SessionProfile) int {
	switch p {
	case ProfileExploring:
		return 80_000
	case ProfileDelegating:
		return 100_000
	case ProfileCoding:
		return 120_000
	case ProfileReasoning:
		return 180_000
	default:
		return 120_000
	}
}

// ContextWarningThreshold returns the threshold above which context size
// is critically large (suggested: bold/red highlight). It is 50% above
// the profile's base threshold.
func ContextWarningThreshold(p SessionProfile) int {
	return ContextThreshold(p) * 3 / 2
}

// ClassifyProfile determines the session profile from aggregate counters.
// totalRecords is the number of records, toolUseRecords is how many had
// tool_use content, subagentRecords had a non-empty AgentID, and
// thinkingTokens/outputTokens are cumulative (outputTokens includes thinking).
func ClassifyProfile(totalRecords, toolUseRecords, subagentRecords, thinkingTokens, outputTokens int) SessionProfile {
	if totalRecords < 3 {
		return ProfileUnknown
	}

	toolRatio := float64(toolUseRecords) / float64(totalRecords)
	thinkRatio := 0.0
	if outputTokens > 0 {
		thinkRatio = float64(thinkingTokens) / float64(outputTokens)
	}
	subagentRatio := float64(subagentRecords) / float64(totalRecords)

	// Delegating: >50% of calls are subagent
	if subagentRatio > 0.5 {
		return ProfileDelegating
	}
	// Reasoning: >40% of output is thinking, low tool use
	if thinkRatio > 0.4 && toolRatio < 0.5 {
		return ProfileReasoning
	}
	// Exploring: >70% of records involve tool use, low thinking
	if toolRatio > 0.7 && thinkRatio < 0.2 {
		return ProfileExploring
	}
	return ProfileCoding
}

// DetectProfile classifies a session based on its records.
func DetectProfile(records []TokenRecord) SessionProfile {
	var total, toolUse, subagent int
	var thinkingTokens, outputTokens int
	for _, r := range records {
		total++
		if strings.Contains(r.ContentType, "tool_use") {
			toolUse++
		}
		if r.AgentID != "" {
			subagent++
		}
		thinkingTokens += r.ThinkingTokens
		outputTokens += r.OutputTokens + r.ThinkingTokens
	}
	return ClassifyProfile(total, toolUse, subagent, thinkingTokens, outputTokens)
}
