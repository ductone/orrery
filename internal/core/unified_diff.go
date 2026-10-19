package core

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the line-alignment table unifiedDiff builds; a larger
// file is shown as wholly replaced.
const maxDiffCells = 4_000_000

type diffOp struct {
	kind byte // ' ', '-', or '+'
	text string
	a, b int // line index in before and after where the op applies
}

func diffLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// unifiedDiff renders a git-style unified diff of one file with context lines
// around each change. A nil before is an added file and a nil after a removed
// one.
func unifiedDiff(path string, before, after []byte, context int) []byte {
	old, cur := diffLines(before), diffLines(after)
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
	switch {
	case before == nil:
		fmt.Fprintf(&b, "new file mode 100644\n--- /dev/null\n+++ b/%s\n", path)
	case after == nil:
		fmt.Fprintf(&b, "deleted file mode 100644\n--- a/%s\n+++ /dev/null\n", path)
	default:
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	}
	ops := diffScript(old, cur)
	var changes []int
	for i, op := range ops {
		if op.kind != ' ' {
			changes = append(changes, i)
		}
	}
	for start := 0; start < len(changes); {
		end := start
		for end+1 < len(changes) && changes[end+1]-changes[end] <= 2*context {
			end++
		}
		lo, hi := max(0, changes[start]-context), min(len(ops)-1, changes[end]+context)
		writeHunk(&b, ops[lo:hi+1])
		start = end + 1
	}
	return []byte(b.String())
}

func writeHunk(b *strings.Builder, ops []diffOp) {
	oldLen, newLen := 0, 0
	for _, op := range ops {
		if op.kind != '+' {
			oldLen++
		}
		if op.kind != '-' {
			newLen++
		}
	}
	oldStart, newStart := ops[0].a+1, ops[0].b+1
	if oldLen == 0 {
		oldStart--
	}
	if newLen == 0 {
		newStart--
	}
	fmt.Fprintf(b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldLen, newStart, newLen)
	for _, op := range ops {
		b.WriteByte(op.kind)
		b.WriteString(op.text)
		b.WriteByte('\n')
	}
}

// diffScript aligns two line slices by longest common subsequence and returns
// the edit script. Past maxDiffCells it deletes every old line and inserts
// every new one.
func diffScript(old, cur []string) []diffOp {
	var ops []diffOp
	if len(old)*len(cur) > maxDiffCells {
		for i, l := range old {
			ops = append(ops, diffOp{kind: '-', text: l, a: i, b: 0})
		}
		for j, l := range cur {
			ops = append(ops, diffOp{kind: '+', text: l, a: len(old), b: j})
		}
		return ops
	}
	// lcs[i][j] is the common subsequence length of old[i:] and cur[j:].
	lcs := make([][]int, len(old)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(cur)+1)
	}
	for i := len(old) - 1; i >= 0; i-- {
		for j := len(cur) - 1; j >= 0; j-- {
			if old[i] == cur[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(old) || j < len(cur) {
		switch {
		case i < len(old) && j < len(cur) && old[i] == cur[j]:
			ops = append(ops, diffOp{kind: ' ', text: old[i], a: i, b: j})
			i, j = i+1, j+1
		case i < len(old) && (j == len(cur) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, diffOp{kind: '-', text: old[i], a: i, b: j})
			i++
		default:
			ops = append(ops, diffOp{kind: '+', text: cur[j], a: i, b: j})
			j++
		}
	}
	return ops
}
