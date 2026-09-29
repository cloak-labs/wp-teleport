// Package state remembers table checksums from previous migrations so
// unchanged tables can be skipped.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cloak-labs/wp-teleport/internal/plan"
)

type Entry struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	Rules string `json:"rules"`
}

type State struct {
	path string
	mu   sync.Mutex
	// Pairs is keyed by "from>to", then source table.
	Pairs map[string]map[string]Entry `json:"pairs"`
}

func Load(dir string) *State {
	s := &State{path: filepath.Join(dir, ".teleport", "state.json"), Pairs: map[string]map[string]Entry{}}
	if raw, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(raw, s)
		if s.Pairs == nil {
			s.Pairs = map[string]map[string]Entry{}
		}
	}
	return s
}

func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	gi := filepath.Join(filepath.Dir(s.path), ".gitignore")
	if _, err := os.Stat(gi); err != nil {
		os.WriteFile(gi, []byte("*\n"), 0o644)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func Key(from, to string) string { return from + ">" + to }

// rewriterVersion must change whenever sqlstream output changes for the same
// input, so tables copied by an older teleport are not treated as in sync.
const rewriterVersion = "2"

// Rules fingerprints everything that changes a table's destination bytes.
func Rules(p *plan.Plan, t plan.TableJob) string {
	h := sha256.New()
	h.Write([]byte(rewriterVersion + "\x00" + t.Live + "\x00" + t.Where + "\x00"))
	for _, pr := range p.Pairs {
		h.Write([]byte(pr.From + "\x00" + pr.To + "\x00"))
	}
	for _, rx := range p.Regexes {
		h.Write([]byte(rx.Pattern.String() + "\x00" + rx.To + "\x00"))
	}
	var coll []string
	for k, v := range p.Collations {
		coll = append(coll, k+"="+v)
	}
	sort.Strings(coll)
	h.Write([]byte(strings.Join(coll, ",")))
	if p.SkipColumns[t.Src]["guid"] {
		h.Write([]byte("skip-guid"))
	}
	if t.Short == "options" {
		h.Write([]byte(strings.Join(p.Preserve, ",")))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// MarkUnchanged flags jobs whose source and destination are both untouched
// since the last migration with identical rules. Returns how many were marked.
func (s *State) MarkUnchanged(key string, p *plan.Plan, srcSums, dstSums map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.Pairs[key]
	n := 0
	for i := range p.Tables {
		t := &p.Tables[i]
		e, ok := prev[t.Src]
		if !ok || e.Src == "" || e.Dst == "" {
			continue
		}
		if srcSums[t.Src] == e.Src && dstSums[t.Live] == e.Dst && Rules(p, *t) == e.Rules {
			t.Unchanged = true
			n++
		}
	}
	return n
}

// Record stores post-migration checksums.
func (s *State) Record(key string, p *plan.Plan, srcSums, dstSums map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Pairs[key] == nil {
		s.Pairs[key] = map[string]Entry{}
	}
	for _, t := range p.Tables {
		if srcSums[t.Src] == "" || dstSums[t.Live] == "" {
			continue
		}
		s.Pairs[key][t.Src] = Entry{Src: srcSums[t.Src], Dst: dstSums[t.Live], Rules: Rules(p, t)}
	}
}
