// Package run owns everything about a single execution of a workflow: its
// directory, its lifecycle, the child process, and the artifacts it leaves.
package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateTimedOut  State = "timed_out"
	StateErrored   State = "errored"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateTimedOut, StateErrored:
		return true
	}
	return false
}

// Artifact kinds. Anything unrecognised is ArtifactOther and still downloadable.
const (
	ArtifactOutputJSON = "output-json"
	ArtifactPolicyCSV  = "policy-csv"
	ArtifactTrace      = "trace"
	ArtifactInputs     = "inputs"
	ArtifactScreenshot = "screenshot"
	ArtifactVideo      = "video"
	ArtifactOther      = "other"
)

type Artifact struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	TestTitle string `json:"testTitle,omitempty"`
	Size      int64  `json:"size"`

	// Sensitive marks artifacts that can contain credentials. Playwright traces
	// record fill() arguments, so a trace of any login holds that user's
	// password in cleartext — there is no per-action redaction to rely on.
	Sensitive bool `json:"sensitive"`

	// Rel is the path relative to the run directory. Clients get the opaque ID
	// instead, so a path never round-trips through a request.
	Rel string `json:"-"`
}

// TestResult is one test as the JSON reporter saw it.
type TestResult struct {
	Title    string `json:"title"`
	Status   string `json:"status"`
	Duration int64  `json:"durationMs"`
	Error    string `json:"error,omitempty"`
}

type Summary struct {
	Total    int          `json:"total"`
	Passed   int          `json:"passed"`
	Failed   int          `json:"failed"`
	Skipped  int          `json:"skipped"`
	Flaky    int          `json:"flaky"`
	Duration int64        `json:"durationMs"`
	Tests    []TestResult `json:"tests"`
}

// Run is the full record of one execution. It is the API's response body and,
// serialised to meta.json, what survives a restart.
type Run struct {
	ID          string `json:"id"`
	WorkflowID  string `json:"workflowId"`
	Env         string `json:"env"`
	SubmittedBy string `json:"submittedBy,omitempty"`

	State State  `json:"state"`
	Error string `json:"error,omitempty"`

	QueuedAt  time.Time  `json:"queuedAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`

	TimeoutSeconds int  `json:"timeoutSeconds"`
	ExitCode       *int `json:"exitCode,omitempty"`

	// Params carries non-secret scalar inputs only, so echoing a Run back can
	// never leak a credential.
	Params map[string]string `json:"params,omitempty"`

	Summary   *Summary   `json:"summary,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`

	// PID of the Playwright process, recorded so a restart can clean up a
	// process tree this server no longer supervises.
	PID int `json:"pid,omitempty"`

	// QueuePosition is filled in on read for queued runs; not persisted.
	QueuePosition int `json:"queuePosition,omitempty"`
}

// Duration is how long the run took, or has been going.
func (r *Run) Duration() time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if r.EndedAt != nil {
		end = *r.EndedAt
	}
	return end.Sub(*r.StartedAt)
}

// NewID returns a sortable, unguessable run id: an RFC3339-ish UTC stamp plus
// 8 bytes of randomness. Sortable so a directory listing is chronological;
// random so ids cannot be enumerated.
func NewID(now time.Time) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating run id: %w", err)
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}
