package watcher

import (
	"os"
	"path/filepath"
	"sync"
	"time"
	"github.com/jlisicki/claude-token-monitor/model"
	"github.com/jlisicki/claude-token-monitor/parser"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	dir        string
	verbose    bool
	debug      bool
	streamAll  bool                    // when true, emit intermediates too (for tail streaming display)
	records    chan []model.TokenRecord
	offsets    map[string]int64
	pending    map[string]*pendingMsg // buffered intermediate entries, keyed by agentID:msgID
	mu         sync.Mutex
	done       chan struct{}
}

func New(dir string, verbose bool, debug bool) *Watcher {
	return &Watcher{
		dir:        dir,
		verbose:    verbose,
		debug:      debug,
		records:    make(chan []model.TokenRecord, 100),
		offsets:    make(map[string]int64),
		pending:    make(map[string]*pendingMsg),
		done:       make(chan struct{}),
	}
}

// SetStreamAll enables pass-through of intermediate streaming entries.
// Must be called before Start.
func (w *Watcher) SetStreamAll(v bool) { w.streamAll = v }

func (w *Watcher) Records() <-chan []model.TokenRecord {
	return w.records
}

func (w *Watcher) InitialScan() (*model.Summary, error) {
	summary := model.NewSummary()
	files, err := parser.FindJSONLFiles(w.dir)
	if err != nil {
		return summary, err
	}

	for _, f := range files {
		recs, offset, err := parser.ParseFileFromOffset(f, 0, false, false)
		if err != nil {
			continue
		}
		w.mu.Lock()
		w.offsets[f] = offset
		recs = w.mergeStreamingEntries(recs)
		w.mu.Unlock()
		for _, r := range recs {
			summary.Add(r)
		}
	}
	// Clear pending buffer — historical state is no longer needed.
	w.mu.Lock()
	w.pending = make(map[string]*pendingMsg)
	w.mu.Unlock()
	return summary, nil
}

// SeekToEnd records current file sizes as offsets without parsing.
// This allows watching for new data without the cost of scanning history.
func (w *Watcher) SeekToEnd() error {
	files, err := parser.FindJSONLFiles(w.dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		w.mu.Lock()
		w.offsets[f] = info.Size()
		w.mu.Unlock()
	}
	return nil
}

const pendingTTL = 1 * time.Minute

// pendingMsg buffers intermediate streaming entries until the final entry arrives.
type pendingMsg struct {
	contentTypes []string
	firstSeen    time.Time
}

// mergeStreamingEntries filters records so that only the final entry per API
// response (identified by stop_reason != "") is emitted, with content types
// merged from all entries.  Intermediate entries are buffered in w.pending.
// Must be called with w.mu held.
func (w *Watcher) mergeStreamingEntries(recs []model.TokenRecord) []model.TokenRecord {
	now := time.Now()

	// Prune expired pending entries to bound memory.
	for key, p := range w.pending {
		if now.Sub(p.firstSeen) > pendingTTL {
			delete(w.pending, key)
		}
	}

	out := recs[:0]
	for _, r := range recs {
		if r.Role != "assistant" || r.MessageID == "" {
			out = append(out, r)
			continue
		}

		key := r.AgentID + ":" + r.MessageID
		p, exists := w.pending[key]

		if r.StopReason == "" {
			// Intermediate entry — buffer content type, don't emit.
			if !exists {
				p = &pendingMsg{firstSeen: now}
				w.pending[key] = p
			}
			if r.ContentType != "" {
				p.contentTypes = append(p.contentTypes, r.ContentType)
			}
			continue
		}

		// Final entry — merge buffered content types and emit.
		if exists {
			all := append(p.contentTypes, r.ContentType)
			r.ContentType = mergeContentTypes(all)
			delete(w.pending, key)
		}
		out = append(out, r)
	}
	return out
}

// mergeContentTypes deduplicates and joins content type strings.
func mergeContentTypes(types []string) string {
	seen := make(map[string]bool)
	var merged []string
	for _, t := range types {
		if t != "" && !seen[t] {
			seen[t] = true
			merged = append(merged, t)
		}
	}
	result := ""
	for i, t := range merged {
		if i > 0 {
			result += "+"
		}
		result += t
	}
	return result
}

func (w *Watcher) processFile(path string) {
	w.mu.Lock()
	offset := w.offsets[path]
	w.mu.Unlock()

	recs, newOffset, err := parser.ParseFileFromOffset(path, offset, w.verbose, w.debug)
	if err != nil {
		return
	}

	w.mu.Lock()
	w.offsets[path] = newOffset
	if !w.streamAll {
		recs = w.mergeStreamingEntries(recs)
	}
	w.mu.Unlock()

	if len(recs) > 0 {
		w.records <- recs
	}
}

func (w *Watcher) Start() {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		go w.pollLoop()
		return
	}

	filepath.Walk(w.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			fsw.Add(path)
		}
		return nil
	})

	go func() {
		defer fsw.Close()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-w.done:
				return
			case event, ok := <-fsw.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					if filepath.Ext(event.Name) == ".jsonl" {
						w.processFile(event.Name)
					}
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						fsw.Add(event.Name)
					}
				}
			case <-ticker.C:
				w.scanNewFiles()
			}
		}
	}()
}

func (w *Watcher) scanNewFiles() {
	files, err := parser.FindJSONLFiles(w.dir)
	if err != nil {
		return
	}
	for _, f := range files {
		w.mu.Lock()
		_, known := w.offsets[f]
		w.mu.Unlock()
		if !known {
			w.processFile(f)
		}
	}
}

func (w *Watcher) pollLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			files, _ := parser.FindJSONLFiles(w.dir)
			for _, f := range files {
				info, err := os.Stat(f)
				if err != nil {
					continue
				}
				w.mu.Lock()
				offset := w.offsets[f]
				w.mu.Unlock()
				if info.Size() > offset {
					w.processFile(f)
				}
			}
		}
	}
}

func (w *Watcher) Stop() {
	close(w.done)
}
