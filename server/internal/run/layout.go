package run

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Layout is one run's private directory tree.
//
//	<root>/
//	  input/        uploaded CSVs (0600)
//	  output/       <- playwright --output=; testInfo.outputPath() lands here
//	  report/       <- PW_HTML_REPORT_DIR
//	  tmp/          <- PS_RUN_TMP_DIR
//	  report.json   <- PW_RESULT_JSON
//	  run.log
//	  meta.json
//
// report/ is a SIBLING of output/, never nested inside it: Playwright refuses
// to start when the HTML reporter's folder is inside the tests' output folder.
type Layout struct{ Root string }

func (l Layout) Input() string      { return filepath.Join(l.Root, "input") }
func (l Layout) Output() string     { return filepath.Join(l.Root, "output") }
func (l Layout) Report() string     { return filepath.Join(l.Root, "report") }
func (l Layout) Tmp() string        { return filepath.Join(l.Root, "tmp") }
func (l Layout) ReportJSON() string { return filepath.Join(l.Root, "report.json") }
func (l Layout) LogFile() string    { return filepath.Join(l.Root, "run.log") }
func (l Layout) MetaFile() string   { return filepath.Join(l.Root, "meta.json") }

// Create makes the directory tree.
func (l Layout) Create() error {
	for _, d := range []string{l.Root, l.Input(), l.Output(), l.Report(), l.Tmp()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}
	return nil
}

// Resolve turns a run-relative path into an absolute one, refusing anything
// that escapes the run directory. Every artifact download goes through here.
func (l Layout) Resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed: %s", rel)
	}
	root, err := filepath.Abs(l.Root)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))

	// filepath.Clean has already collapsed any "..", so a prefix check is
	// sufficient — and the separator guard stops "/runs/abc" matching "/runs/abcd".
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes run directory: %s", rel)
	}
	return full, nil
}

// RelTo expresses an absolute path inside the run directory as a slash-separated
// relative path, suitable for storing in meta.json on any platform.
func (l Layout) RelTo(abs string) (string, error) {
	root, err := filepath.Abs(l.Root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is outside the run directory", abs)
	}
	return filepath.ToSlash(rel), nil
}
