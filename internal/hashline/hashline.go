package hashline

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Line struct {
	Number int    `json:"number"`
	Hash   string `json:"hash"`
	Text   string `json:"text"`
}
type Hunk struct {
	Anchor                string   `json:"anchor"`
	Offset                int      `json:"offset"`
	Delete                int      `json:"delete"`
	Insert                []string `json:"insert"`
	AllowStructuralChange bool     `json:"allow_structural_change,omitempty"`
}
type Patch struct {
	Path  string `json:"path"`
	Hunks []Hunk `json:"hunks"`
}
type StaleError struct {
	Anchor string
	Fresh  []Line
}

func (e *StaleError) Error() string { return fmt.Sprintf("stale or ambiguous anchor %q", e.Anchor) }
func hash(s string) string          { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:8] }
func Read(path string) ([]Line, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, 8<<20)
	for sc.Scan() {
		t := sc.Text()
		out = append(out, Line{len(out) + 1, hash(t), t})
	}
	return out, sc.Err()
}
func Apply(p Patch) error {
	lines, err := Read(p.Path)
	if err != nil {
		return err
	}
	raw := make([]string, len(lines))
	for i, l := range lines {
		raw[i] = l.Text
	}
	type located struct {
		at int
		h  Hunk
	}
	loc := make([]located, 0, len(p.Hunks))
	for _, h := range p.Hunks {
		at := -1
		for i, l := range lines {
			if strings.HasPrefix(l.Hash, h.Anchor) {
				if at != -1 {
					return &StaleError{h.Anchor, window(lines, i)}
				}
				at = i
			}
		}
		if at < 0 {
			return &StaleError{h.Anchor, window(lines, 0)}
		}
		at += h.Offset
		if at < 0 || at > len(raw) || h.Delete < 0 || at+h.Delete > len(raw) {
			return errors.New("hashline: hunk range out of bounds")
		}
		if !h.AllowStructuralChange {
			for _, deleted := range raw[at : at+h.Delete] {
				decl := declarationKey(deleted)
				if decl != "" && !containsDeclaration(h.Insert, decl) {
					return fmt.Errorf("hashline: refusing to delete declaration %q without preserving it; set allow_structural_change=true for intentional removal", decl)
				}
			}
		}
		loc = append(loc, located{at, h})
	}
	for i := 1; i < len(loc); i++ {
		if loc[i].at < loc[i-1].at {
			return errors.New("hashline: hunks must be ordered")
		}
	}
	for i := len(loc) - 1; i >= 0; i-- {
		x := loc[i]
		raw = append(raw[:x.at], append(x.h.Insert, raw[x.at+x.h.Delete:]...)...)
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		return err
	}
	return os.WriteFile(p.Path, []byte(strings.Join(raw, "\n")+"\n"), info.Mode())
}

func declarationKey(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return ""
	}
	if fields[0] == "export" && len(fields) >= 3 {
		fields = fields[1:]
	}
	switch fields[0] {
	case "func", "type", "class", "interface", "const", "let", "var", "function":
		return fields[0] + " " + strings.Trim(fields[1], "({[:=,")
	}
	return ""
}

func containsDeclaration(lines []string, key string) bool {
	for _, line := range lines {
		if declarationKey(line) == key {
			return true
		}
	}
	return false
}
func window(lines []Line, at int) []Line {
	lo := max(0, at-2)
	hi := min(len(lines), at+3)
	return lines[lo:hi]
}
