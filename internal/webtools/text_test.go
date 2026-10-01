package webtools

import (
	"strings"
	"testing"
)

const docPage = `<!DOCTYPE html><html><head><title>Memory | Docs</title>
<style>.x{color:red}</style><script>window.__DATA__={"huge":"payload"}</script></head>
<body>
<header><nav><a href="/">Home</a> <a href="/docs">Docs</a></nav></header>
<aside>Table of contents</aside>
<main>
  <h1>Long-term memory</h1>
  <p>Agents   store   facts
     across sessions.</p>
  <ul><li>Semantic memory</li><li>Episodic memory</li></ul>
  <pre>store.put(("user", "1"), "prefs", {"theme": "dark"})
    indented line</pre>
  <div hidden>secret hidden text</div>
  <svg><text>chart label</text></svg>
</main>
<footer>Copyright</footer>
<script>more()</script>
</body></html>`

func TestHTMLTextKeepsReadingTextOnly(t *testing.T) {
	title, text := htmlText([]byte(docPage))
	if title != "Memory | Docs" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{"# Long-term memory", "Agents store facts across sessions.", "- Semantic memory", "- Episodic memory", "store.put((\"user\", \"1\")", "    indented line"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"__DATA__", "color:red", "Home", "Table of contents", "Copyright", "secret hidden", "chart label", "more()"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("text must not contain %q:\n%s", unwanted, text)
		}
	}
}

func TestHTMLTextWithoutMainUsesTheBody(t *testing.T) {
	_, text := htmlText([]byte(`<html><body><nav>menu</nav><h2>Title</h2><p>Body text.</p><table><tr><td>a</td><td>b</td></tr></table></body></html>`))
	if !strings.Contains(text, "## Title") || !strings.Contains(text, "Body text.") || !strings.Contains(text, "a b") || strings.Contains(text, "menu") {
		t.Fatalf("text = %q", text)
	}
}

func TestHTMLTextShrinksMarkupHeavyPages(t *testing.T) {
	page := "<html><head>" + strings.Repeat("<script>var x = '"+strings.Repeat("y", 1000)+"';</script>", 2000) + "</head><body><main><p>The actual content.</p></main></body></html>"
	_, text := htmlText([]byte(page))
	if text != "The actual content." {
		t.Fatalf("a 2MB page of scripts must reduce to its text, got %d chars", len(text))
	}
}

func TestTextWindow(t *testing.T) {
	line := strings.Repeat("x", 99) + "\n"
	text := strings.Repeat(line, 1500) // 150K chars
	first := textWindow(text, 0)
	content := first["content"].(string)
	if len(content) > FetchChars || !strings.HasSuffix(content, "\n") || first["total_chars"] != len(text) {
		t.Fatalf("first window: %d chars, total %v", len(content), first["total_chars"])
	}
	next := first["next_start"].(int)
	second := textWindow(text, next)
	if second["content"].(string)[:5] != "xxxxx" || second["next_start"] == nil {
		t.Fatalf("second window = %v", second["next_start"])
	}
	last := textWindow(text, len(text)-50)
	if _, more := last["next_start"]; more || len(last["content"].(string)) != 50 {
		t.Fatal("the last window has no continuation")
	}
	if got := textWindow("short", 999); got["content"] != "" {
		t.Fatalf("a start past the end is empty: %v", got)
	}
}
