// Package receipts lists the curated attachment library.
//
// The library is shared and read-only: these are test fixtures, not per-user
// documents, so every run resolves attName against the same directory. Exposing
// the list is what lets the Excel templates offer a dropdown instead of a free
// text field — which turns "typo'd filename fails forty minutes into a run" into
// something you cannot express.
package receipts

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type Receipt struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type Library struct{ Dir string }

func New(dir string) *Library { return &Library{Dir: dir} }

// List returns the available attachments, sorted by name. One level only —
// attName is joined directly to the receipts dir, so subdirectories would not
// be addressable anyway.
func (l *Library) List() ([]Receipt, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Receipt{}, nil
		}
		return nil, fmt.Errorf("reading receipts dir: %w", err)
	}

	out := make([]Receipt, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || skip(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Receipt{
			Name:       e.Name(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Has reports whether a referenced attachment exists. Used to reject a CSV at
// upload time rather than mid-run.
func (l *Library) Has(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	fi, err := os.Stat(l.Dir + string(os.PathSeparator) + name)
	return err == nil && !fi.IsDir()
}

// ETag is a cheap validator over the listing: count plus newest mtime.
func ETag(rs []Receipt) string {
	var newest int64
	for _, r := range rs {
		if t := r.ModifiedAt.UnixNano(); t > newest {
			newest = t
		}
	}
	return fmt.Sprintf(`W/"%d-%d"`, len(rs), newest)
}

func skip(name string) bool {
	return strings.HasPrefix(name, ".") || strings.EqualFold(name, "README.md")
}
