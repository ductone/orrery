package hashline

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

type Line struct {
	Number int    `json:"number"`
	Hash   string `json:"hash"`
	Text   string `json:"text"`
}
type Hunk struct {
	Anchor string `json:"anchor"`
	// Line is the anchor's line number as the latest read showed it. It is
	// optional, and only consulted when the anchor matches more than one line.
	Line                  int      `json:"line,omitempty"`
	Offset                int      `json:"offset"`
	Delete                int      `json:"delete"`
	Insert                []string `json:"insert"`
	AllowStructuralChange bool     `json:"allow_structural_change,omitempty"`
}
type Patch struct {
	Path  string `json:"path"`
	Hunks []Hunk `json:"hunks"`
}

type AnchorMode string

const (
	AnchorLine       AnchorMode = "line"
	AnchorContextual AnchorMode = "contextual"
	AnchorText       AnchorMode = "text"
)

type StaleError struct {
	Anchor string
	Fresh  []Line
}

var ErrNoChanges = errors.New("hashline: patch makes no changes")

func (e *StaleError) Error() string {
	return fmt.Sprintf("stale anchor %q: no line in the current file has this hash", e.Anchor)
}

// AmbiguousError reports an anchor that matches several lines, as identical
// lines do, and which the hunk's line number did not settle. It is not stale:
// every listed line is current.
type AmbiguousError struct {
	Anchor  string
	Lines   []int
	Windows [][]Line
	// NewFileAnchor marks the empty-line hash, which creates files but also
	// matches every blank line of an existing one.
	NewFileAnchor bool
}

func (e *AmbiguousError) Error() string {
	nums := make([]string, len(e.Lines))
	for i, n := range e.Lines {
		nums[i] = fmt.Sprint(n)
	}
	msg := fmt.Sprintf("ambiguous anchor %q: it matches lines %s", e.Anchor, strings.Join(nums, ", "))
	if e.NewFileAnchor {
		return msg + "; this is the new-file anchor, which on an existing file matches every blank line. Anchor to a line with content instead"
	}
	return msg + `; set "line" to the intended line number from your read, or anchor to a nearby unique line with an offset`
}

// lineTolerance bounds how far a line hint may be from its match when the
// file has shifted since the read.
const lineTolerance = 40

// pickOccurrence settles an ambiguous anchor with the hunk's line hint. An
// exact line match always wins. Otherwise an insert may take the nearest match
// when it is within tolerance and clearly nearer than the next; a delete must
// match exactly, since deleting at a near guess would destroy the wrong lines.
func pickOccurrence(occurrences []int, line int, deleting bool) (int, bool) {
	if line <= 0 {
		return 0, false
	}
	want := line - 1
	for _, idx := range occurrences {
		if idx == want {
			return idx, true
		}
	}
	if deleting {
		return 0, false
	}
	best, second := -1, -1
	for _, idx := range occurrences {
		d := abs(idx - want)
		if best < 0 || d < abs(best-want) {
			best, second = idx, best
		} else if second < 0 || d < abs(second-want) {
			second = idx
		}
	}
	if d := abs(best - want); d <= lineTolerance && (second < 0 || 2*d < abs(second-want)) {
		return best, true
	}
	return 0, false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func ambiguous(anchor string, lines []Line, occurrences []int, newFileAnchor bool) *AmbiguousError {
	e := &AmbiguousError{Anchor: anchor, NewFileAnchor: newFileAnchor}
	for _, idx := range occurrences {
		e.Lines = append(e.Lines, idx+1)
		if len(e.Windows) < 6 {
			e.Windows = append(e.Windows, window(lines, idx))
		}
	}
	return e
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:8] }

func hashes(text []string, mode AnchorMode) []Line {
	out := make([]Line, len(text))
	for i, current := range text {
		anchorText := current
		if mode == AnchorText {
			out[i] = Line{Number: i + 1, Hash: current, Text: current}
			continue
		}
		if mode == AnchorContextual {
			previous, next := "", ""
			if i > 0 {
				previous = text[i-1]
			}
			if i+1 < len(text) {
				next = text[i+1]
			}
			anchorText = previous + "\x00" + current + "\x00" + next
		}
		out[i] = Line{Number: i + 1, Hash: hash(anchorText), Text: current}
	}
	return out
}

func Read(path string) ([]Line, error) { return ReadWithMode(path, AnchorLine) }

func ReadWithMode(path string, mode AnchorMode) ([]Line, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var text []string
	sc := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, 8<<20)
	for sc.Scan() {
		text = append(text, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return hashes(text, mode), nil
}

type AffectedRegion struct {
	Start int
	End   int
}

type ApplyResult struct {
	Lines    []Line
	Affected []AffectedRegion
}

func Apply(p Patch) (*ApplyResult, error) { return ApplyWithMode(p, AnchorLine) }

func ApplyWithMode(p Patch, mode AnchorMode) (*ApplyResult, error) {
	lines, err := ReadWithMode(p.Path, mode)
	newFile := errors.Is(err, os.ErrNotExist)
	if err != nil && !newFile {
		return nil, err
	}
	raw := make([]string, len(lines))
	for i, l := range lines {
		raw[i] = l.Text
	}
	original := append([]string(nil), raw...)
	type located struct {
		at int
		h  Hunk
	}
	loc := make([]located, 0, len(p.Hunks))
	for _, h := range p.Hunks {
		// Find all occurrences of anchor in the snapshot
		var occurrences []int
		newFileAnchor := hash("")
		if mode == AnchorText {
			newFileAnchor = ""
		}
		if len(lines) == 0 && h.Anchor == newFileAnchor && h.Offset == 0 && h.Delete == 0 {
			occurrences = []int{0}
		} else {
			for i, l := range lines {
				matched := strings.HasPrefix(l.Hash, h.Anchor)
				if mode == AnchorText {
					matched = l.Hash == h.Anchor
				}
				if matched {
					occurrences = append(occurrences, i)
				}
			}
		}

		if len(occurrences) == 0 {
			return nil, &StaleError{h.Anchor, window(lines, 0)}
		}

		// Identical lines share a hash. The read's line number settles which
		// one was meant; without it, a delete must not guess (that destroys
		// information) and an insert may only use a uniquely in-bounds match.
		if len(occurrences) > 1 {
			if idx, ok := pickOccurrence(occurrences, h.Line, h.Delete > 0); ok {
				occurrences = []int{idx}
			} else if h.Delete > 0 {
				return nil, ambiguous(h.Anchor, lines, occurrences, h.Anchor == newFileAnchor)
			}
		}

		// Filter to occurrences that give valid targets
		var validOccurrences []int
		for _, idx := range occurrences {
			target := idx + h.Offset
			if target >= 0 && target <= len(raw) && h.Delete >= 0 && target+h.Delete <= len(raw) {
				validOccurrences = append(validOccurrences, idx)
			}
		}

		var at int
		if len(validOccurrences) == 0 {
			// No in-bounds target. For a delete this is always a hard stale error
			// (never relocate a destructive hunk). For an insert, the anchor is stale.
			return nil, &StaleError{h.Anchor, window(lines, occurrences[0])}
		}

		if len(validOccurrences) > 1 {
			return nil, ambiguous(h.Anchor, lines, validOccurrences, h.Anchor == newFileAnchor)
		}

		// Exactly one valid target - use it (auto-rebase if needed)
		at = validOccurrences[0]
		at += h.Offset

		if !h.AllowStructuralChange {
			for _, deleted := range raw[at : at+h.Delete] {
				decl := declarationKey(deleted)
				if decl != "" && !containsDeclaration(h.Insert, decl) {
					return nil, fmt.Errorf("hashline: refusing to delete declaration %q without preserving it; set allow_structural_change=true for intentional removal", decl)
				}
			}
		}
		loc = append(loc, located{at, h})
	}
	// Hunks resolve against the same snapshot, so their order in the patch
	// carries no meaning; apply them in file order.
	slices.SortStableFunc(loc, func(a, b located) int { return a.at - b.at })
	for i := 1; i < len(loc); i++ {
		if loc[i].at < loc[i-1].at+loc[i-1].h.Delete {
			return nil, errors.New("hashline: overlapping delete ranges")
		}
	}
	// Affected regions are in the new file's coordinates: a hunk moves by the
	// net line change of the hunks above it, and spans the lines it inserted
	// (an empty span where it only deleted).
	affected := make([]AffectedRegion, 0, len(loc))
	delta := 0
	for _, x := range loc {
		start := x.at + delta
		affected = append(affected, AffectedRegion{start, start + len(x.h.Insert)})
		delta += len(x.h.Insert) - x.h.Delete
	}
	// Apply bottom-up so earlier positions stay valid.
	for i := len(loc) - 1; i >= 0; i-- {
		x := loc[i]
		raw = append(raw[:x.at], append(x.h.Insert, raw[x.at+x.h.Delete:]...)...)
	}
	if slices.Equal(raw, original) {
		return nil, ErrNoChanges
	}
	fileMode := os.FileMode(0o644)
	if !newFile {
		info, statErr := os.Stat(p.Path)
		if statErr != nil {
			return nil, statErr
		}
		fileMode = info.Mode()
	}
	if err := os.WriteFile(p.Path, []byte(strings.Join(raw, "\n")+"\n"), fileMode); err != nil {
		return nil, err
	}
	newLines := hashes(raw, mode)
	return &ApplyResult{Lines: newLines, Affected: affected}, nil
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
