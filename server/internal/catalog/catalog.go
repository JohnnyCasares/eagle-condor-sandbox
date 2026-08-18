// Package catalog reads automation/workflows.json — the same file the Python
// client reads. Neither side owns it; the file does.
//
// Validation here is deliberately strict and happens at startup: a manifest
// that names a spec which no longer exists should stop the server booting, not
// surface as a confusing failure forty minutes into someone's run.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Module struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// Input is one thing a workflow needs supplied. Env is the load-bearing field:
// it is how "this workflow needs a TA spreadsheet" becomes
// "the spec reads process.env.MASTER_CSV".
type Input struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // csv | int
	Required bool   `json:"required"`
	Env      string `json:"env"`
	Filename string `json:"filename"`
	Schema   string `json:"schema"`
	Label    string `json:"label"`
	Help     string `json:"help"`

	// Scalar inputs only.
	Default *int `json:"default,omitempty"`
	Min     *int `json:"min,omitempty"`
	Max     *int `json:"max,omitempty"`

	// Client-side concerns the server passes through untouched.
	Template         string `json:"template,omitempty"`
	Parser           string `json:"parser,omitempty"`
	TemplateFilename string `json:"templateFilename,omitempty"`
	TemplateHelp     string `json:"templateHelp,omitempty"`
	Group            string `json:"group,omitempty"`
	Caption          string `json:"caption,omitempty"`
	Note             string `json:"note,omitempty"`
}

type ResultFile struct {
	Glob  string `json:"glob"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type Results struct {
	OutputJSON bool         `json:"outputJson"`
	Files      []ResultFile `json:"files"`
}

type Workflow struct {
	ID                    string  `json:"id"`
	Module                string  `json:"module"`
	Label                 string  `json:"label"`
	Description           string  `json:"description"`
	Spec                  string  `json:"spec"`
	Class                 string  `json:"class"`
	DefaultTimeoutSeconds int     `json:"defaultTimeoutSeconds"`
	MaxTimeoutSeconds     int     `json:"maxTimeoutSeconds"`
	Inputs                []Input `json:"inputs"`
	Results               Results `json:"results"`
}

// Input returns the named input, or false.
func (w Workflow) Input(name string) (Input, bool) {
	for _, in := range w.Inputs {
		if in.Name == name {
			return in, true
		}
	}
	return Input{}, false
}

type CSVSchema struct {
	Headers       []string `json:"headers"`
	SecretColumns []string `json:"secretColumns"`
}

// IsSecret reports whether a column must never be echoed back to a client.
func (s CSVSchema) IsSecret(col string) bool {
	for _, c := range s.SecretColumns {
		if strings.EqualFold(c, col) {
			return true
		}
	}
	return false
}

type Catalog struct {
	Version    int                  `json:"version"`
	Modules    []Module             `json:"modules"`
	CSVSchemas map[string]CSVSchema `json:"csvSchemas"`
	Workflows  []Workflow           `json:"workflows"`

	byID map[string]*Workflow
	raw  []byte
}

// Raw is the manifest bytes as read from disk, served verbatim so a client on
// another machine needs no checkout.
func (c *Catalog) Raw() []byte { return c.raw }

func (c *Catalog) Workflow(id string) (*Workflow, bool) {
	w, ok := c.byID[id]
	return w, ok
}

func (c *Catalog) Schema(name string) (CSVSchema, bool) {
	s, ok := c.CSVSchemas[name]
	return s, ok
}

// Load reads and validates the manifest. automationDir is used to confirm each
// spec exists.
func Load(manifestPath, automationDir string) (*Catalog, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	// The manifest documents itself with "$comment" keys, whose values are
	// arrays of prose. Those have to come out before typed decoding, or a
	// $comment sitting alongside real entries in a map (csvSchemas) fails to
	// unmarshal. The Python loader strips them the same way.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	cleaned, err := json.Marshal(stripComments(generic))
	if err != nil {
		return nil, fmt.Errorf("normalising manifest: %w", err)
	}

	var c Catalog
	// Not DisallowUnknownFields: the manifest also carries client-only hints
	// (template, parser, captions) that the server has no business rejecting.
	if err := json.Unmarshal(cleaned, &c); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	// Raw stays the original bytes: GET /v1/workflows serves the file as
	// authored, comments included.
	c.raw = raw

	if err := c.index(automationDir); err != nil {
		return nil, err
	}
	return &c, nil
}

// stripComments removes every "$comment" key, recursively.
func stripComments(node any) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if k == "$comment" {
				continue
			}
			out[k] = stripComments(val)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = stripComments(val)
		}
		return out
	default:
		return node
	}
}

func (c *Catalog) index(automationDir string) error {
	c.byID = make(map[string]*Workflow, len(c.Workflows))

	modules := make(map[string]bool, len(c.Modules))
	for _, m := range c.Modules {
		modules[m.Code] = true
	}

	var problems []string
	for i := range c.Workflows {
		w := &c.Workflows[i]

		if w.ID == "" {
			problems = append(problems, fmt.Sprintf("workflow #%d: missing id", i))
			continue
		}
		if _, dup := c.byID[w.ID]; dup {
			problems = append(problems, w.ID+": duplicate id")
		}
		c.byID[w.ID] = w

		if !modules[w.Module] {
			problems = append(problems, fmt.Sprintf("%s: unknown module %q", w.ID, w.Module))
		}
		if w.Spec == "" {
			problems = append(problems, w.ID+": missing spec")
		} else if _, err := os.Stat(filepath.Join(automationDir, filepath.FromSlash(w.Spec))); err != nil {
			problems = append(problems, fmt.Sprintf("%s: spec not found: %s", w.ID, w.Spec))
		}
		if w.DefaultTimeoutSeconds <= 0 {
			problems = append(problems, w.ID+": defaultTimeoutSeconds must be positive")
		}
		if w.MaxTimeoutSeconds < w.DefaultTimeoutSeconds {
			problems = append(problems, w.ID+": maxTimeoutSeconds is below defaultTimeoutSeconds")
		}

		seenEnv := map[string]bool{}
		seenName := map[string]bool{}
		for _, in := range w.Inputs {
			ref := w.ID + "." + in.Name
			if in.Name == "" {
				problems = append(problems, w.ID+": input with no name")
				continue
			}
			if seenName[in.Name] {
				problems = append(problems, ref+": duplicate input name")
			}
			seenName[in.Name] = true

			switch in.Kind {
			case "csv":
				if in.Schema == "" {
					problems = append(problems, ref+": csv input needs a schema")
				} else if _, ok := c.CSVSchemas[in.Schema]; !ok {
					problems = append(problems, fmt.Sprintf("%s: unknown csvSchema %q", ref, in.Schema))
				}
				if in.Filename == "" {
					problems = append(problems, ref+": csv input needs a filename")
				}
			case "int":
			default:
				problems = append(problems, fmt.Sprintf("%s: unsupported kind %q", ref, in.Kind))
			}

			if in.Env == "" {
				problems = append(problems, ref+": missing env")
			} else if seenEnv[in.Env] {
				problems = append(problems, fmt.Sprintf("%s: env %s used twice in one workflow", ref, in.Env))
			}
			seenEnv[in.Env] = true
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("manifest is inconsistent with the repo:\n  - %s",
			strings.Join(problems, "\n  - "))
	}
	return nil
}
