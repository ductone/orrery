package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFooterUsesSeparateIdentityAndUsageRows(t *testing.T) {
	m := newModel(context.Background(), Options{
		Create: CreateRequest{Workspace: "/repo"},
	}, "session-12345678")
	m.width = 40
	m.st.model = "openai/gpt-5.6-terra"
	m.st.effort = "high"
	m.st.tokensIn = 1200
	m.st.tokensOut = 345
	m.st.cost = 1.25

	rows := m.footer()
	if len(rows) != 2 {
		t.Fatalf("footer returned %d rows, want 2", len(rows))
	}
	identity, usage := ansi.Strip(rows[0]), ansi.Strip(rows[1])
	for _, want := range []string{"/repo", "session-", "gpt-5.6-terra", "high"} {
		if !strings.Contains(identity, want) {
			t.Errorf("identity row %q does not contain %q", identity, want)
		}
	}
	if strings.Contains(identity, "↑1.2K") || strings.Contains(identity, "$1.25") {
		t.Errorf("identity row contains usage fields: %q", identity)
	}
	for _, want := range []string{"↑1.2K ↓345", "$1.25", "remote"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage row %q does not contain %q", usage, want)
		}
	}
	if strings.Contains(usage, "gpt-5.6-terra") {
		t.Errorf("usage row contains model: %q", usage)
	}
	for i, row := range rows {
		if got := ansi.StringWidth(row); got != m.width {
			t.Errorf("row %d width = %d, want %d", i, got, m.width)
		}
	}
}

func TestFooterRowTruncatesRowsIndependently(t *testing.T) {
	const width = 20
	identity := footerRow([]string{"repo", "gpt-5.6-terra"}, nil, " · ", width)
	usage := footerRow(nil, []string{"↑123K ↓45K", "$12.34", "remote"}, " · ", width)

	if got := ansi.Strip(identity); !strings.Contains(got, "gpt-5.6-") || !strings.Contains(got, "…") {
		t.Fatalf("identity row does not keep a visible truncated model: %q", got)
	}
	if strings.Contains(ansi.Strip(identity), "remote") {
		t.Fatalf("identity row contains usage content: %q", ansi.Strip(identity))
	}
	for name, row := range map[string]string{"identity": identity, "usage": usage} {
		if got := ansi.StringWidth(row); got != width {
			t.Errorf("%s row width = %d, want %d", name, got, width)
		}
	}
}
