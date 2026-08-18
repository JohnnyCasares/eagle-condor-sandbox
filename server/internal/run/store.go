package run

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store keeps every run in memory and mirrors each to <runDir>/meta.json, so a
// restart does not lose the history. meta.json is written with write-temp-then-
// rename, so a crash mid-write leaves the previous version intact rather than a
// truncated file.
type Store struct {
	runsDir string

	mu   sync.RWMutex
	runs map[string]*Run

	// persistMu serialises the write-temp-then-rename dance.
	//
	// A run is persisted from two goroutines within milliseconds — the HTTP
	// handler on Add, then a worker on the queued->running transition. On
	// Windows, two concurrent renames onto the same destination fail with
	// "Access is denied", which is exactly what happened before this lock. Held
	// separately from mu so persistence never blocks readers.
	persistMu sync.Mutex
}

func NewStore(runsDir string) *Store {
	return &Store{runsDir: runsDir, runs: make(map[string]*Run)}
}

func (s *Store) layout(id string) Layout {
	return Layout{Root: filepath.Join(s.runsDir, id)}
}

// Layout exposes a run's directory tree.
func (s *Store) Layout(id string) Layout { return s.layout(id) }

// Add registers a new run and persists it.
func (s *Store) Add(r *Run) error {
	s.mu.Lock()
	s.runs[r.ID] = r
	s.mu.Unlock()
	return s.persist(r)
}

// Get returns a copy. Callers get a snapshot rather than a live pointer so they
// cannot mutate shared state by accident.
func (s *Store) Get(id string) (*Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// Update applies mutate under lock and persists the result.
func (s *Store) Update(id string, mutate func(*Run)) (*Run, error) {
	s.mu.Lock()
	r, ok := s.runs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("no run %s", id)
	}
	mutate(r)
	cp := *r
	s.mu.Unlock()

	if err := s.persist(&cp); err != nil {
		return &cp, err
	}
	return &cp, nil
}

// List returns runs newest-first, optionally filtered.
func (s *Store) List(state State, workflowID string, limit int) []*Run {
	s.mu.RLock()
	out := make([]*Run, 0, len(s.runs))
	for _, r := range s.runs {
		if state != "" && r.State != state {
			continue
		}
		if workflowID != "" && r.WorkflowID != workflowID {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt.After(out[j].QueuedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Counts reports how many runs are in each of the two live states.
func (s *Store) Counts() (running, queued int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.runs {
		switch r.State {
		case StateRunning:
			running++
		case StateQueued:
			queued++
		}
	}
	return
}

func (s *Store) persist(r *Run) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	lay := s.layout(r.ID)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding meta for %s: %w", r.ID, err)
	}

	tmp, err := os.CreateTemp(lay.Root, "meta-*.json")
	if err != nil {
		return fmt.Errorf("meta temp for %s: %w", r.ID, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing meta for %s: %w", r.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing meta for %s: %w", r.ID, err)
	}

	// Even serialised, a rename can lose a race with a virus scanner or the
	// search indexer briefly holding the destination open. Those clear in
	// milliseconds, so retry rather than losing the state update.
	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		if renameErr = os.Rename(tmpName, lay.MetaFile()); renameErr == nil {
			return nil
		}
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
	}
	return fmt.Errorf("replacing meta for %s: %w", r.ID, renameErr)
}

// Recover rebuilds the store from disk at startup.
//
// Any run still marked queued or running belongs to a previous process that is
// no longer supervising it, so it is moved to errored and its recorded process
// tree is killed. Without this, a restart leaves orphaned Chromium processes
// and runs that never leave "running".
func (s *Store) Recover(log *slog.Logger) error {
	entries, err := os.ReadDir(s.runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("scanning runs dir: %w", err)
	}

	var recovered, orphaned int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		lay := s.layout(e.Name())
		data, err := os.ReadFile(lay.MetaFile())
		if err != nil {
			continue // not a run directory, or never got as far as meta.json
		}

		var r Run
		if err := json.Unmarshal(data, &r); err != nil {
			log.Warn("skipping unreadable run metadata", "run", e.Name(), "err", err)
			continue
		}
		if r.ID == "" {
			r.ID = e.Name()
		}

		if !r.State.Terminal() {
			orphaned++
			if r.PID > 0 {
				if err := KillTree(r.PID); err != nil {
					log.Warn("could not kill orphaned process tree",
						"run", r.ID, "pid", r.PID, "err", err)
				} else {
					log.Info("killed orphaned process tree", "run", r.ID, "pid", r.PID)
				}
			}
			r.State = StateErrored
			r.Error = "server restarted while this run was " + string(r.State)
			if r.EndedAt == nil {
				now := time.Now()
				r.EndedAt = &now
			}
			s.runs[r.ID] = &r
			if err := s.persist(&r); err != nil {
				log.Warn("could not persist recovered run", "run", r.ID, "err", err)
			}
			continue
		}

		s.runs[r.ID] = &r
		recovered++
	}

	log.Info("recovered runs from disk", "terminal", recovered, "orphaned", orphaned)
	return nil
}
