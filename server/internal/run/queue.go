package run

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"eagle-condor-sandbox/internal/catalog"
	"eagle-condor-sandbox/internal/logbuf"
)

var (
	// ErrQueueFull means the backlog is at capacity; the caller should retry.
	ErrQueueFull = errors.New("run queue is full")
	// ErrNotFound is returned when cancelling an unknown run.
	ErrNotFound = errors.New("no such run")
	// ErrAlreadyFinished means the run reached a terminal state already.
	ErrAlreadyFinished = errors.New("run has already finished")
)

type job struct {
	runID    string
	spec     Spec
	buf      *logbuf.Buffer
	cancelFn context.CancelFunc
}

// Queue is a bounded FIFO with a fixed pool of workers.
//
// Each worker runs one Playwright process at a time. A workflow that fans out
// into multiple browser contexts per run would make this number a floor, not
// the real parallelism figure — see server/README.md's Capacity section.
type Queue struct {
	store   *Store
	runtime Runtime
	log     *slog.Logger
	workers int

	jobs chan *job

	mu       sync.Mutex
	live     map[string]*job // queued or running
	bufs     map[string]*logbuf.Buffer
	cancelled map[string]bool // cancelled before a worker picked it up

	wg       sync.WaitGroup
	stopOnce sync.Once
}

func NewQueue(store *Store, rt Runtime, workers, depth int, log *slog.Logger) *Queue {
	return &Queue{
		store:     store,
		runtime:   rt,
		log:       log,
		workers:   workers,
		jobs:      make(chan *job, depth),
		live:      make(map[string]*job),
		bufs:      make(map[string]*logbuf.Buffer),
		cancelled: make(map[string]bool),
	}
}

// Start launches the worker pool.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker(ctx, i)
	}
	q.log.Info("run queue started", "workers", q.workers, "depth", cap(q.jobs))
}

// Stop closes the queue and waits for in-flight runs to finish.
func (q *Queue) Stop() {
	q.stopOnce.Do(func() { close(q.jobs) })
	q.wg.Wait()
}

// Buffer returns the live log buffer for a run, if it is still in memory.
func (q *Queue) Buffer(runID string) (*logbuf.Buffer, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	b, ok := q.bufs[runID]
	return b, ok
}

// Submit enqueues a run. The run must already exist in the store as queued, and
// its input files must already be on disk — so a crash loses only work that was
// in flight, never a request we acknowledged.
func (q *Queue) Submit(r *Run, wf *catalog.Workflow, env map[string]string) error {
	buf := logbuf.New(5000)

	j := &job{
		runID: r.ID,
		spec: Spec{
			Workflow:       wf,
			Layout:         q.store.Layout(r.ID),
			Env:            env,
			TimeoutSeconds: r.TimeoutSeconds,
		},
		buf: buf,
	}

	q.mu.Lock()
	q.live[r.ID] = j
	q.bufs[r.ID] = buf
	q.mu.Unlock()

	select {
	case q.jobs <- j:
		return nil
	default:
		q.mu.Lock()
		delete(q.live, r.ID)
		delete(q.bufs, r.ID)
		q.mu.Unlock()
		buf.Close()
		return ErrQueueFull
	}
}

// Cancel stops a run. A queued run is flagged and skipped when it reaches the
// front; a running one has its process tree killed.
func (q *Queue) Cancel(runID string) error {
	r, ok := q.store.Get(runID)
	if !ok {
		return ErrNotFound
	}
	if r.State.Terminal() {
		return ErrAlreadyFinished
	}

	q.mu.Lock()
	j, live := q.live[runID]
	q.cancelled[runID] = true
	q.mu.Unlock()

	if !live {
		return ErrNotFound
	}

	if j.cancelFn != nil {
		// Running: the exec goroutine sees the context cancel and kills the tree.
		j.cancelFn()
		return nil
	}

	// Still queued: mark it terminal now so the caller sees it immediately.
	now := time.Now()
	_, err := q.store.Update(runID, func(r *Run) {
		r.State = StateCancelled
		r.EndedAt = &now
	})
	return err
}

// QueuePosition reports how many queued runs sit ahead of this one.
func (q *Queue) QueuePosition(runID string) int {
	queued := q.store.List(StateQueued, "", 0)
	// List is newest-first; queue order is oldest-first.
	pos := 0
	for i := len(queued) - 1; i >= 0; i-- {
		if queued[i].ID == runID {
			return pos + 1
		}
		pos++
	}
	return 0
}

func (q *Queue) worker(ctx context.Context, n int) {
	defer q.wg.Done()
	log := q.log.With("worker", n)

	for j := range q.jobs {
		q.mu.Lock()
		skip := q.cancelled[j.runID]
		q.mu.Unlock()

		if skip {
			log.Info("skipping cancelled run", "run", j.runID)
			q.finish(j, Outcome{State: StateCancelled, ExitCode: -1})
			continue
		}

		q.execute(ctx, log, j)
	}
}

func (q *Queue) execute(ctx context.Context, log *slog.Logger, j *job) {
	runCtx, cancel := context.WithCancel(ctx)
	q.mu.Lock()
	j.cancelFn = cancel
	q.mu.Unlock()
	defer cancel()

	now := time.Now()
	if _, err := q.store.Update(j.runID, func(r *Run) {
		r.State = StateRunning
		r.StartedAt = &now
	}); err != nil {
		log.Error("could not mark run running", "run", j.runID, "err", err)
	}

	log.Info("run started", "run", j.runID, "spec", j.spec.Workflow.Spec,
		"timeout", j.spec.TimeoutSeconds)

	outcome := Execute(runCtx, q.runtime, j.spec, j.buf, func(pid int) {
		if _, err := q.store.Update(j.runID, func(r *Run) { r.PID = pid }); err != nil {
			log.Warn("could not record pid", "run", j.runID, "err", err)
		}
	})

	q.finish(j, outcome)
	log.Info("run finished", "run", j.runID, "state", outcome.State, "exit", outcome.ExitCode)
}

func (q *Queue) finish(j *job, outcome Outcome) {
	summary, artifacts := Collect(j.spec.Layout)

	now := time.Now()
	exit := outcome.ExitCode
	if _, err := q.store.Update(j.runID, func(r *Run) {
		// A cancellation recorded while the process was already exiting wins:
		// the user asked for it, and the exit code is incidental.
		if r.State != StateCancelled || outcome.State == StateCancelled {
			r.State = outcome.State
		}
		r.EndedAt = &now
		r.ExitCode = &exit
		r.Summary = summary
		r.Artifacts = artifacts
		r.PID = 0
		if outcome.Err != nil {
			r.Error = outcome.Err.Error()
		}
	}); err != nil {
		q.log.Error("could not finalise run", "run", j.runID, "err", err)
	}

	j.buf.Append("[server] run " + string(outcome.State))
	j.buf.Close()

	q.mu.Lock()
	delete(q.live, j.runID)
	delete(q.cancelled, j.runID)
	// The buffer stays in q.bufs so a client polling /log can read the tail
	// after the run ends. It is bounded, and the full log is on disk.
	q.mu.Unlock()
}
