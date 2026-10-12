package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ductone/orrey/internal/store"
)

func plainTodoLines(lines []string) string {
	return ansi.Strip(strings.Join(lines, "\n"))
}

func TestTodoBlockFirstWriteShowsFullList(t *testing.T) {
	r := newRenderer()
	cur := []store.Todo{
		{Text: "Add parser tests", Phase: "implement", Status: "in_progress"},
		{Text: "Wire into engine", Phase: "implement", Status: "pending"},
	}
	got := plainTodoLines(r.todoBlockLines(nil, cur))
	if !strings.Contains(got, "Add parser tests") || !strings.Contains(got, "Wire into engine") {
		t.Fatalf("first write should list every item, got %q", got)
	}
	if strings.Contains(got, "implement") {
		t.Fatalf("phase column should be gone, got %q", got)
	}
}

func TestTodoBlockStatusChangeIsOneTransitionLine(t *testing.T) {
	r := newRenderer()
	prev := []store.Todo{
		{Text: "Add parser tests", Phase: "implement", Status: "in_progress"},
		{Text: "Wire into engine", Phase: "implement", Status: "pending"},
		{Text: "Write docs", Phase: "wrap-up", Status: "pending"},
	}
	cur := []store.Todo{
		{Text: "Add parser tests", Phase: "implement", Status: "completed"},
		{Text: "Wire into engine", Phase: "implement", Status: "in_progress"},
		{Text: "Write docs", Phase: "wrap-up", Status: "pending"},
	}
	lines := r.todoBlockLines(prev, cur)
	if len(lines) != 1 {
		t.Fatalf("status-only update should be one line, got %d: %q", len(lines), plainTodoLines(lines))
	}
	got := plainTodoLines(lines)
	if !strings.Contains(got, "Add parser tests") || !strings.Contains(got, "Wire into engine") {
		t.Fatalf("transition line should name changed items, got %q", got)
	}
	if strings.Contains(got, "Write docs") {
		t.Fatalf("unchanged items should not repeat, got %q", got)
	}
	if !strings.Contains(got, "(1/3)") {
		t.Fatalf("transition line should show progress, got %q", got)
	}
}

func TestTodoBlockRestructureShowsFullList(t *testing.T) {
	r := newRenderer()
	prev := []store.Todo{
		{Text: "A", Status: "in_progress"},
		{Text: "B", Status: "pending"},
	}
	cur := []store.Todo{
		{Text: "A", Status: "completed"},
		{Text: "B", Status: "pending"},
		{Text: "C", Status: "pending"},
	}
	lines := r.todoBlockLines(prev, cur)
	if len(lines) != 3 {
		t.Fatalf("an added item is a restructure and should list all items, got %q", plainTodoLines(lines))
	}
}

func TestTodoBlockNoChangeRendersNothing(t *testing.T) {
	r := newRenderer()
	same := []store.Todo{{Text: "A", Status: "pending"}}
	if lines := r.todoBlockLines(same, same); len(lines) != 0 {
		t.Fatalf("an update with no status change should render nothing, got %q", plainTodoLines(lines))
	}
}
