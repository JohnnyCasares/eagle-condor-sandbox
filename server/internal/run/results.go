package run

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// The subset of Playwright's JSON reporter output we care about.
//
// Durations are float64, not int64: the reporter emits fractional milliseconds
// ("duration": 69067.84599999999). Declaring them as int64 makes the whole
// document fail to unmarshal, which silently produced a nil summary on every
// run until it was caught.
type pwReport struct {
	Stats struct {
		Expected   int     `json:"expected"`
		Unexpected int     `json:"unexpected"`
		Skipped    int     `json:"skipped"`
		Flaky      int     `json:"flaky"`
		Duration   float64 `json:"duration"`
	} `json:"stats"`
	Suites []pwSuite `json:"suites"`
}

type pwSuite struct {
	Title  string    `json:"title"`
	Specs  []pwSpec  `json:"specs"`
	Suites []pwSuite `json:"suites"`
}

type pwSpec struct {
	Title string   `json:"title"`
	Ok    bool     `json:"ok"`
	Tests []pwTest `json:"tests"`
}

type pwTest struct {
	Status  string     `json:"status"`
	Results []pwResult `json:"results"`
}

type pwResult struct {
	Status   string  `json:"status"`
	Duration float64 `json:"duration"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Collect reads back everything a finished run produced.
//
// This replaces the previous approach of globbing test-results/ and filtering on
// file mtime, which could not tell two concurrent runs apart. Here every path is
// already inside this run's own directory, so there is nothing to disambiguate.
func Collect(lay Layout) (*Summary, []Artifact) {
	summary := readSummary(lay.ReportJSON())
	artifacts := scanArtifacts(lay)
	return summary, artifacts
}

func readSummary(reportPath string) *Summary {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil // reporter never ran (e.g. the process died at startup)
	}
	var rep pwReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil
	}

	s := &Summary{
		Passed:   rep.Stats.Expected,
		Failed:   rep.Stats.Unexpected,
		Skipped:  rep.Stats.Skipped,
		Flaky:    rep.Stats.Flaky,
		Duration: int64(math.Round(rep.Stats.Duration)),
	}
	var walk func(suite pwSuite, prefix string)
	walk = func(suite pwSuite, prefix string) {
		title := strings.TrimPrefix(prefix+" › "+suite.Title, " › ")
		for _, sp := range suite.Specs {
			tr := TestResult{Title: strings.TrimPrefix(title+" › "+sp.Title, " › ")}
			for _, t := range sp.Tests {
				tr.Status = t.Status
				for _, r := range t.Results {
					tr.Duration += int64(math.Round(r.Duration))
					if r.Error.Message != "" && tr.Error == "" {
						tr.Error = r.Error.Message
					}
					if r.Status != "" {
						tr.Status = r.Status
					}
				}
			}
			s.Tests = append(s.Tests, tr)
		}
		for _, child := range suite.Suites {
			walk(child, title)
		}
	}
	for _, suite := range rep.Suites {
		walk(suite, "")
	}
	s.Total = len(s.Tests)
	return s
}

// scanArtifacts walks the run's output directory. Everything Playwright wrote
// is under it because of --output=, so a plain walk is both sufficient and
// unambiguous.
func scanArtifacts(lay Layout) []Artifact {
	var out []Artifact

	root := lay.Output()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry should not abort the walk
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if isInternal(d.Name()) {
			return nil
		}
		rel, err := lay.RelTo(p)
		if err != nil {
			return nil
		}
		out = append(out, newArtifact(rel, info.Size()))
		return nil
	})

	// The JSON report itself is useful to download whole.
	if info, err := os.Stat(lay.ReportJSON()); err == nil {
		if rel, err := lay.RelTo(lay.ReportJSON()); err == nil {
			out = append(out, newArtifact(rel, info.Size()))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// isInternal filters Playwright's own bookkeeping out of the artifact list.
// .last-run.json records which tests to re-run with --last-failed; it is noise
// to anyone downloading results.
func isInternal(name string) bool {
	return name == ".last-run.json" || strings.HasPrefix(name, ".playwright")
}

func newArtifact(rel string, size int64) Artifact {
	name := path.Base(rel)
	kind := ArtifactOther
	sensitive := false

	switch {
	case name == "output.json":
		kind = ArtifactOutputJSON
	case name == "policyViolations.csv":
		kind = ArtifactPolicyCSV
	case name == "inputs.json":
		kind = ArtifactInputs
	case name == "report.json":
		kind = ArtifactOther
	case strings.HasPrefix(name, "trace") && strings.HasSuffix(name, ".zip"):
		kind = ArtifactTrace
		// Playwright records fill() arguments, so a trace covering a login
		// contains that user's password in cleartext.
		sensitive = true
	case strings.HasSuffix(name, ".png"), strings.HasSuffix(name, ".jpg"):
		kind = ArtifactScreenshot
	case strings.HasSuffix(name, ".webm"):
		kind = ArtifactVideo
	}

	// Directory name Playwright derives from the test title, which is the most
	// useful label available for grouping artifacts in a UI.
	testTitle := ""
	if dir := path.Dir(rel); dir != "." && dir != "output" {
		testTitle = path.Base(dir)
	}

	return Artifact{
		ID:        artifactID(rel),
		Kind:      kind,
		Name:      name,
		TestTitle: testTitle,
		Size:      size,
		Sensitive: sensitive,
		Rel:       rel,
	}
}

// artifactID is a stable opaque handle for a path, so clients never send a path
// back to us and the download handler has nothing to sanitise beyond a lookup.
func artifactID(rel string) string {
	sum := sha1.Sum([]byte(rel))
	return hex.EncodeToString(sum[:8])
}
