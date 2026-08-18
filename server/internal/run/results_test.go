package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testdata/report_sample.json is a real report from a create-domestic run,
// trimmed of the config blob and long stack traces. Its stats.duration is
// 69067.84599999999 — a fractional millisecond, which is the whole point: the
// duration fields were once declared int64, so the entire document failed to
// unmarshal and every run reported a nil summary.
func TestReadSummaryParsesFractionalDurations(t *testing.T) {
	s := readSummary(filepath.Join("testdata", "report_sample.json"))
	if s == nil {
		t.Fatal("readSummary returned nil for a valid report — " +
			"check that duration fields are float64, not int64")
	}

	if s.Failed != 1 || s.Passed != 0 {
		t.Errorf("stats: got passed=%d failed=%d, want passed=0 failed=1", s.Passed, s.Failed)
	}
	if s.Duration != 69068 {
		t.Errorf("duration: got %d, want 69068 (69067.846 rounded)", s.Duration)
	}
	if s.Total != len(s.Tests) {
		t.Errorf("Total (%d) should equal len(Tests) (%d)", s.Total, len(s.Tests))
	}
	if s.Total == 0 {
		t.Fatal("no tests extracted from the suites tree")
	}
	if s.Tests[0].Title == "" {
		t.Error("test title is empty — suite titles are not being joined")
	}
	if s.Tests[0].Error == "" {
		t.Error("expected the failing test to carry its error message")
	}
}

func TestReadSummaryMissingFile(t *testing.T) {
	if got := readSummary(filepath.Join("testdata", "does-not-exist.json")); got != nil {
		t.Errorf("want nil for a missing report, got %+v", got)
	}
}

func TestReadSummaryMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "report.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readSummary(p); got != nil {
		t.Errorf("want nil for a malformed report, got %+v", got)
	}
}

// Summary must survive a JSON round-trip, since it is both persisted to
// meta.json and served over the API.
func TestSummaryRoundTrips(t *testing.T) {
	s := readSummary(filepath.Join("testdata", "report_sample.json"))
	if s == nil {
		t.Fatal("fixture did not parse")
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Summary
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Duration != s.Duration || back.Total != s.Total {
		t.Errorf("round trip changed the summary: %+v vs %+v", back, *s)
	}
}

func TestIsInternalFiltersPlaywrightBookkeeping(t *testing.T) {
	for _, name := range []string{".last-run.json", ".playwright-artifacts-0"} {
		if !isInternal(name) {
			t.Errorf("%s should be filtered from the artifact list", name)
		}
	}
	for _, name := range []string{"output.json", "trace.zip", "policyViolations.csv"} {
		if isInternal(name) {
			t.Errorf("%s must NOT be filtered", name)
		}
	}
}

func TestArtifactClassification(t *testing.T) {
	cases := []struct {
		rel       string
		kind      string
		sensitive bool
	}{
		{"output/some-test/output.json", ArtifactOutputJSON, false},
		{"output/some-test/policyViolations.csv", ArtifactPolicyCSV, false},
		// Traces record fill() arguments, so they hold cleartext passwords.
		{"output/some-test/trace.zip", ArtifactTrace, true},
		{"output/some-test/inputs.json", ArtifactInputs, false},
		{"output/some-test/screenshot.png", ArtifactScreenshot, false},
		{"output/some-test/notes.txt", ArtifactOther, false},
	}
	for _, c := range cases {
		a := newArtifact(c.rel, 10)
		if a.Kind != c.kind {
			t.Errorf("%s: kind=%s want %s", c.rel, a.Kind, c.kind)
		}
		if a.Sensitive != c.sensitive {
			t.Errorf("%s: sensitive=%v want %v", c.rel, a.Sensitive, c.sensitive)
		}
		if a.ID == "" {
			t.Errorf("%s: artifact needs a stable id", c.rel)
		}
	}
}

// Artifact ids are handed to clients in place of paths, so the same path must
// always map to the same id and different paths must not collide.
func TestArtifactIDStability(t *testing.T) {
	a := newArtifact("output/x/output.json", 1)
	b := newArtifact("output/x/output.json", 999)
	if a.ID != b.ID {
		t.Error("same path produced different ids")
	}
	c := newArtifact("output/y/output.json", 1)
	if a.ID == c.ID {
		t.Error("different paths collided")
	}
}
