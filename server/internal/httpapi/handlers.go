package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"eagle-condor-sandbox/internal/catalog"
	"eagle-condor-sandbox/internal/receipts"
	"eagle-condor-sandbox/internal/run"
)

// ---------------------------------------------------------------------------
// health / catalog
// ---------------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	running, queued := s.store.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"pool": map[string]int{
			"running":  running,
			"queued":   queued,
			"capacity": s.cfg.MaxConcurrent,
		},
	})
}

// listWorkflows serves the manifest verbatim, so a client on another machine
// needs no checkout to know what it can run.
func (s *Server) listWorkflows(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.catalog.Raw())
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.catalog.Workflow(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such workflow")
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) listReceipts(w http.ResponseWriter, r *http.Request) {
	list, err := s.receipts.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	etag := receipts.ETag(list)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, map[string]any{"receipts": list})
}

// getReference passes through captured reference data, if a workflow writes
// any, so a client can build its own UI without a repo checkout.
func (s *Server) getReference(w http.ResponseWriter, _ *http.Request) {
	path := filepath.Join(s.cfg.AutomationDir, "data", "shared", "reference", "ps-reference.json")
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "reference data not captured yet")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.Copy(w, f)
}

// ---------------------------------------------------------------------------
// runs
// ---------------------------------------------------------------------------

type createConfig struct {
	WorkflowID     string            `json:"workflowId"`
	Env            string            `json:"env"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Trace          bool              `json:"trace"`
	SubmittedBy    string            `json:"submittedBy"`
	Params         map[string]string `json:"params"`
}

var validEnvs = map[string]bool{"DEV": true, "TST": true, "STG": true}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	// Bound the whole request, not just one part.
	maxTotal := s.cfg.MaxUploadBytes * 8
	r.Body = http.MaxBytesReader(w, r.Body, maxTotal)

	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest,
			"expected multipart/form-data with a 'config' part: "+err.Error())
		return
	}

	var cfg createConfig
	uploads := map[string][]byte{}
	gotConfig := false

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "reading upload: "+err.Error())
			return
		}

		name := part.FormName()
		if name == "config" {
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&cfg); err != nil {
				writeError(w, http.StatusBadRequest, "config is not valid JSON: "+err.Error())
				return
			}
			gotConfig = true
			part.Close()
			continue
		}

		data, err := io.ReadAll(io.LimitReader(part, s.cfg.MaxUploadBytes+1))
		part.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, "reading part "+name+": "+err.Error())
			return
		}
		if int64(len(data)) > s.cfg.MaxUploadBytes {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("part %q exceeds %d bytes", name, s.cfg.MaxUploadBytes))
			return
		}
		uploads[name] = data
	}

	if !gotConfig {
		writeError(w, http.StatusBadRequest, "missing 'config' part")
		return
	}

	wf, ok := s.catalog.Workflow(cfg.WorkflowID)
	if !ok {
		writeError(w, http.StatusBadRequest, "no such workflow: "+cfg.WorkflowID)
		return
	}

	env := strings.ToUpper(cfg.Env)
	if env == "" {
		env = "TST"
	}
	if !validEnvs[env] {
		writeError(w, http.StatusBadRequest, "env must be one of DEV, TST, STG")
		return
	}

	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = wf.DefaultTimeoutSeconds
	}
	if wf.MaxTimeoutSeconds > 0 && timeout > wf.MaxTimeoutSeconds {
		timeout = wf.MaxTimeoutSeconds
	}

	// Reject unknown parts rather than silently ignoring a typo'd input name.
	for name := range uploads {
		if _, ok := wf.Input(name); !ok {
			writeError(w, http.StatusBadRequest, "unknown input: "+name)
			return
		}
	}

	// Build the run directory before enqueuing, so a queued run is fully
	// self-describing on disk.
	id, err := run.NewID(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lay := s.store.Layout(id)
	if err := lay.Create(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	childEnv := map[string]string{"ENV": env}
	if cfg.Trace {
		childEnv["PW_TRACE"] = "on"
	}
	params := map[string]string{}

	for _, in := range wf.Inputs {
		switch in.Kind {
		case "csv":
			data, present := uploads[in.Name]
			if !present {
				if in.Required {
					writeError(w, http.StatusBadRequest,
						fmt.Sprintf("missing required input %q (%s)", in.Name, in.Label))
					_ = os.RemoveAll(lay.Root)
					return
				}
				continue
			}
			if err := s.validateCSV(wf, in, data); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				_ = os.RemoveAll(lay.Root)
				return
			}
			dest := filepath.Join(lay.Input(), in.Filename)
			if err := os.WriteFile(dest, data, 0o600); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				_ = os.RemoveAll(lay.Root)
				return
			}
			childEnv[in.Env] = dest

		case "int":
			raw, present := cfg.Params[in.Name]
			if !present {
				if in.Default != nil {
					raw = fmt.Sprintf("%d", *in.Default)
				} else if in.Required {
					writeError(w, http.StatusBadRequest, "missing required param "+in.Name)
					_ = os.RemoveAll(lay.Root)
					return
				} else {
					continue
				}
			}
			childEnv[in.Env] = raw
			params[in.Name] = raw
		}
	}

	r0 := &run.Run{
		ID:             id,
		WorkflowID:     wf.ID,
		Env:            env,
		SubmittedBy:    cfg.SubmittedBy,
		State:          run.StateQueued,
		QueuedAt:       time.Now(),
		TimeoutSeconds: timeout,
		Params:         params,
	}
	if err := s.store.Add(r0); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.queue.Submit(r0, wf, childEnv); err != nil {
		if errors.Is(err, run.ErrQueueFull) {
			_, _ = s.store.Update(id, func(r *run.Run) {
				r.State = run.StateErrored
				r.Error = "queue full"
			})
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "run queue is full; retry shortly")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Location", "/v1/runs/"+id)
	writeJSON(w, http.StatusAccepted, r0)
}

// validateCSV checks the upload against the manifest's schema before the run is
// ever queued, and checks referenced receipts exist. Both failures used to
// surface deep inside a run instead of at submit time.
func (s *Server) validateCSV(wf *catalog.Workflow, in catalog.Input, data []byte) error {
	schema, ok := s.catalog.Schema(in.Schema)
	if !ok {
		return fmt.Errorf("input %q names unknown schema %q", in.Name, in.Schema)
	}

	rd := csv.NewReader(strings.NewReader(string(data)))
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil {
		return fmt.Errorf("input %q is not valid CSV: %w", in.Name, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("input %q is empty", in.Name)
	}

	hdrIdx, present, missing := findHeaderRow(rows, schema.Headers)
	if hdrIdx < 0 {
		return fmt.Errorf("input %q has no recognisable header row (expected the %s columns)",
			in.Name, in.Schema)
	}
	if len(missing) > 0 {
		return fmt.Errorf("input %q is missing column(s): %s (expected the %s template)",
			in.Name, strings.Join(missing, ", "), in.Schema)
	}

	// Attachment columns must name a file in the shared library. Rejecting here
	// is the entire point of the library being enumerable: otherwise a typo'd
	// filename surfaces as a file-chooser error deep inside a long run.
	for _, col := range []string{"attName", "attachmentFile"} {
		idx, ok := present[col]
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, row := range rows[hdrIdx+1:] {
			if idx >= len(row) {
				continue
			}
			name := strings.TrimSpace(row[idx])
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if !s.receipts.Has(name) {
				return fmt.Errorf(
					"input %q references receipt %q, which is not in the attachment library "+
						"(GET /v1/receipts lists what is available)", in.Name, name)
			}
		}
	}
	return nil
}

// headerScanRows is how far in to look for the header row.
const headerScanRows = 10

// findHeaderRow locates the row holding the column names.
//
// Several templates carry a merged section-label row above the headers
// ("Scenario \u2014 Group ID, TA creator credentials, \u2026"), so the header row is not
// always row 0 \u2014 modules/ta/csvDataHandler.js scans for it the same way. Rather
// than hardcode an offset per template, take the best-matching row and report
// what it is missing, which gives a useful error for a genuinely wrong file too.
func findHeaderRow(rows [][]string, want []string) (idx int, present map[string]int, missing []string) {
	bestIdx, bestScore := -1, -1
	var bestPresent map[string]int

	limit := min(len(rows), headerScanRows)
	for i := 0; i < limit; i++ {
		cols := map[string]int{}
		for j, h := range rows[i] {
			// Excel writes a UTF-8 BOM ahead of the first header when saving CSV.
			cols[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = j
		}
		score := 0
		for _, w := range want {
			if _, ok := cols[w]; ok {
				score++
			}
		}
		if score > bestScore {
			bestIdx, bestScore, bestPresent = i, score, cols
		}
		if score == len(want) {
			break // exact match, stop looking
		}
	}

	if bestIdx < 0 || bestScore == 0 {
		return -1, nil, nil
	}
	for _, w := range want {
		if _, ok := bestPresent[w]; !ok {
			missing = append(missing, w)
		}
	}
	return bestIdx, bestPresent, missing
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.store.List(
		run.State(r.URL.Query().Get("state")),
		r.URL.Query().Get("workflow"),
		queryInt(r, "limit", 50),
	)
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	if rec.State == run.StateQueued {
		rec.QueuePosition = s.queue.QueuePosition(rec.ID)
	}
	writeJSON(w, http.StatusOK, rec)
}

// getLog is the primary way clients follow a run.
//
// Polling rather than streaming is deliberate for now: Streamlit's rerun model
// cannot hold an SSE connection open, and it is the first client. `from` makes
// it resumable, so nothing is missed between polls.
func (s *Server) getLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}

	from := queryInt64(r, "from", 0)
	limit := queryInt(r, "limit", 2000)

	buf, live := s.queue.Buffer(id)
	if !live {
		// Buffer evicted (e.g. after a restart) — fall back to the file.
		lines, next := tailFile(s.store.Layout(id).LogFile(), from, limit)
		writeJSON(w, http.StatusOK, map[string]any{
			"lines": lines, "nextSeq": next, "state": rec.State, "source": "file",
		})
		return
	}

	lines, next := buf.Since(from, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines, "nextSeq": next, "state": rec.State, "source": "memory",
	})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch err := s.queue.Cancel(id); {
	case err == nil:
		rec, _ := s.store.Get(id)
		writeJSON(w, http.StatusAccepted, rec)
	case errors.Is(err, run.ErrAlreadyFinished):
		writeError(w, http.StatusConflict, "run has already finished")
	case errors.Is(err, run.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such run")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) getResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}

	lay := s.store.Layout(id)
	outputs := []map[string]any{}
	for _, a := range rec.Artifacts {
		if a.Kind != run.ArtifactOutputJSON {
			continue
		}
		full, err := lay.Resolve(a.Rel)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err == nil {
			outputs = append(outputs, parsed)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":        rec.ID,
		"state":     rec.State,
		"summary":   rec.Summary,
		"outputs":   outputs,
		"artifacts": rec.Artifacts,
	})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": rec.Artifacts})
}

// getArtifact serves one file. Clients hold an opaque id, never a path, and the
// resolved path is re-checked against the run directory before opening.
func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}

	wanted := r.PathValue("artifactID")
	var found *run.Artifact
	for i := range rec.Artifacts {
		if rec.Artifacts[i].ID == wanted {
			found = &rec.Artifacts[i]
			break
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, "no such artifact")
		return
	}

	full, err := s.store.Layout(id).Resolve(found.Rel)
	if err != nil {
		s.log.Warn("rejected artifact path", "run", id, "rel", found.Rel, "err", err)
		writeError(w, http.StatusBadRequest, "invalid artifact path")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact file is gone")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Disposition", `attachment; filename="`+found.Name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	if found.Sensitive {
		// Traces embed credentials; keep them out of any shared cache.
		w.Header().Set("Cache-Control", "no-store, private")
	}
	http.ServeContent(w, r, found.Name, time.Time{}, f)
}

// tailFile reads run.log when the in-memory buffer is gone, numbering lines
// from zero so `from` keeps the same meaning as the memory path.
func tailFile(path string, from int64, limit int) ([]map[string]any, int64) {
	data, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}, from
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := []map[string]any{}
	for i, text := range all {
		seq := int64(i)
		if seq < from {
			continue
		}
		out = append(out, map[string]any{"seq": seq, "text": text})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	next := int64(len(all))
	if len(out) > 0 {
		next = out[len(out)-1]["seq"].(int64) + 1
	}
	return out, next
}
