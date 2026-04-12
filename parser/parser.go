package parser

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"github.com/jlisicki/claude-token-monitor/model"
)

type contentBlock struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content,omitempty"`
	Text    string          `json:"text,omitempty"`
}

type jsonLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	AgentID   string `json:"agentId"`
	Message   *struct {
		ID      string         `json:"id"`
		Model   string         `json:"model"`
		Content []contentBlock `json:"content"`
		Usage   *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreation            *struct {
				Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
		StopReason *string `json:"stop_reason"`
	} `json:"message"`
}

func projectFromCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	base := filepath.Base(cwd)
	// If the base name is very short/generic, include the parent dir for context
	if len(base) <= 3 || base == "project" || base == "src" || base == "app" || base == "code" {
		parent := filepath.Base(filepath.Dir(cwd))
		if parent != "" && parent != "." && parent != "/" {
			return parent + "/" + base
		}
	}
	return base
}

func joinContentTypes(blocks []contentBlock) string {
	types := make(map[string]bool)
	for _, b := range blocks {
		if b.Type != "" {
			types[b.Type] = true
		}
	}
	var ct string
	for t := range types {
		if ct != "" {
			ct += "+"
		}
		ct += t
	}
	return ct
}

func rawLineIf(line []byte, keep bool) string {
	if keep {
		return string(line)
	}
	return ""
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ParseLine(line []byte) *model.TokenRecord {
	return parseLine(line, false)
}

func parseLine(line []byte, keepRaw bool) *model.TokenRecord {
	var jl jsonLine
	if err := json.Unmarshal(line, &jl); err != nil {
		return nil
	}
	if jl.Type != "assistant" || jl.Message == nil || jl.Message.Usage == nil {
		return nil
	}
	if strings.Contains(jl.Message.Model, "synthetic") {
		return nil
	}

	ts, _ := time.Parse(time.RFC3339, jl.Timestamp)

	contentType := joinContentTypes(jl.Message.Content)

	isThinking := false
	for _, block := range jl.Message.Content {
		if block.Type == "thinking" {
			isThinking = true
			break
		}
	}

	var thinkingTokens int
	outputTokens := jl.Message.Usage.OutputTokens
	if isThinking {
		thinkingTokens = outputTokens
		outputTokens = 0
	}

	var cacheWrite5m, cacheWrite1h int
	if cc := jl.Message.Usage.CacheCreation; cc != nil {
		cacheWrite5m = cc.Ephemeral5m
		cacheWrite1h = cc.Ephemeral1h
	}

	return &model.TokenRecord{
		Role:                "assistant",
		Model:               jl.Message.Model,
		InputTokens:         jl.Message.Usage.InputTokens,
		OutputTokens:        outputTokens,
		ThinkingTokens:      thinkingTokens,
		CacheCreationTokens: jl.Message.Usage.CacheCreationInputTokens,
		CacheWrite5m:        cacheWrite5m,
		CacheWrite1h:        cacheWrite1h,
		CacheReadTokens:     jl.Message.Usage.CacheReadInputTokens,
		ContentType:         contentType,
		Timestamp:           ts,
		SessionID:           jl.SessionID,
		AgentID:             jl.AgentID,
		Project:             projectFromCWD(jl.CWD),
		RawLine:             rawLineIf(line, keepRaw),
		MessageID:           jl.Message.ID,
		StopReason:          derefStr(jl.Message.StopReason),
	}
}

// verboseLine is a minimal struct for parsing non-assistant lines.
type verboseLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	AgentID   string `json:"agentId"`
	Message   *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// ParseVerboseLine parses user/system lines that have no token usage.
func parseVerboseLine(line []byte, keepRaw bool) *model.TokenRecord {
	var vl verboseLine
	if err := json.Unmarshal(line, &vl); err != nil {
		return nil
	}
	if vl.Type != "user" && vl.Type != "system" {
		return nil
	}
	if vl.Message == nil {
		return nil
	}

	ts, _ := time.Parse(time.RFC3339, vl.Timestamp)

	contentType := ""
	contentSize := len(vl.Message.Content)

	var blocks []contentBlock
	if err := json.Unmarshal(vl.Message.Content, &blocks); err == nil {
		contentType = joinContentTypes(blocks)
		size := 0
		for _, b := range blocks {
			size += len(b.Content) + len(b.Text)
		}
		contentSize = size
	} else {
		contentType = "text"
	}

	return &model.TokenRecord{
		Role:        vl.Type,
		ContentType: contentType,
		ContentSize: contentSize,
		Timestamp:   ts,
		SessionID:   vl.SessionID,
		AgentID:     vl.AgentID,
		Project:     projectFromCWD(vl.CWD),
		RawLine:     rawLineIf(line, keepRaw),
	}
}

// dedup merges multiple JSONL entries from the same API response into one record.
//
// Claude Code logs one entry per content block as it streams.  Intermediate
// entries have stop_reason=null and carry partial output_tokens; only the final
// entry (stop_reason != "") has the correct total.  We keep only the final
// entry per message.id, merging content types from all entries so the display
// shows e.g. "thinking+text+tool_use".
func dedup(records []model.TokenRecord) []model.TokenRecord {
	type group struct {
		finalIdx     int
		contentTypes []string
	}
	// Keyed by agentID:messageID to avoid collisions between main and subagents.
	groups := make(map[string]*group)

	for i := range records {
		r := &records[i]
		if r.Role != "assistant" || r.MessageID == "" {
			continue
		}
		key := r.AgentID + ":" + r.MessageID
		g, ok := groups[key]
		if !ok {
			g = &group{finalIdx: -1}
			groups[key] = g
		}
		if r.ContentType != "" {
			g.contentTypes = append(g.contentTypes, r.ContentType)
		}
		if r.StopReason != "" {
			g.finalIdx = i
		}
	}

	// Mark which indices to keep (final entries + non-assistant records).
	keep := make([]bool, len(records))
	for i := range records {
		r := &records[i]
		if r.Role != "assistant" || r.MessageID == "" {
			keep[i] = true
			continue
		}
		key := r.AgentID + ":" + r.MessageID
		g := groups[key]
		if g.finalIdx == i {
			// Merge content types from all entries in this group.
			r.ContentType = mergeContentTypes(g.contentTypes)
			keep[i] = true
		}
		// If no final entry exists yet (shouldn't happen in complete files),
		// keep the last entry as fallback.
		if g.finalIdx == -1 {
			keep[i] = true
			g.finalIdx = i
		}
	}

	out := records[:0]
	for i, r := range records {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

// mergeContentTypes deduplicates and joins content type strings.
// Input: ["thinking", "text", "tool_use", "tool_use"] → "thinking+text+tool_use"
func mergeContentTypes(types []string) string {
	seen := make(map[string]bool)
	var merged []string
	for _, t := range types {
		if t != "" && !seen[t] {
			seen[t] = true
			merged = append(merged, t)
		}
	}
	return strings.Join(merged, "+")
}

func ParseFile(path string) ([]model.TokenRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseReader(f)
}

func ParseReader(r io.Reader) ([]model.TokenRecord, error) {
	var records []model.TokenRecord
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		if rec := ParseLine(scanner.Bytes()); rec != nil {
			records = append(records, *rec)
		}
	}
	return dedup(records), scanner.Err()
}

// ParseFileFromOffset reads new lines from offset, returns records and new offset.
// If verbose is true, also includes user/system lines.
// If debug is true, retains the raw JSONL line on each record.
func ParseFileFromOffset(path string, offset int64, verbose bool, debug bool) ([]model.TokenRecord, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, offset, err
		}
	}

	var records []model.TokenRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		b := scanner.Bytes()
		offset += int64(len(b)) + 1
		if rec := parseLine(b, debug); rec != nil {
			records = append(records, *rec)
		} else if verbose {
			if rec := parseVerboseLine(b, debug); rec != nil {
				records = append(records, *rec)
			}
		}
	}
	return records, offset, scanner.Err()
}

// FindJSONLFiles finds all .jsonl files recursively under dir.
func FindJSONLFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") && !strings.Contains(filepath.Base(path), "history") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}
