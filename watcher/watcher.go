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
	dir      string
	verbose  bool
	debug    bool
	records  chan []model.TokenRecord
	offsets  map[string]int64
	mu       sync.Mutex
	done     chan struct{}
}

func New(dir string, verbose bool, debug bool) *Watcher {
	return &Watcher{
		dir:     dir,
		verbose: verbose,
		debug:   debug,
		records: make(chan []model.TokenRecord, 100),
		offsets: make(map[string]int64),
		done:    make(chan struct{}),
	}
}

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
		w.mu.Unlock()
		for _, r := range recs {
			summary.Add(r)
		}
	}
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
