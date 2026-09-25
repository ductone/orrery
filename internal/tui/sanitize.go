package tui

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Event payloads carry file contents, command output, and model text, none
// of which may drive the terminal. Printed lines reach the terminal without
// the renderer's filtering, so control sequences in them could rewrite
// scrollback, set the clipboard (OSC 52), spoof the title, or forge the
// screen markers Squire reads. Payloads are therefore cleaned when an event
// is applied, before anything is styled.

// clean keeps newlines and tabs, turns ESC into a visible ␛, carriage
// returns into newlines, and drops every other C0, DEL, and C1 control.
func clean(s string) string {
	if !needsClean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			if i+1 >= len(s) || s[i+1] != '\n' {
				b.WriteByte('\n')
			}
		case r == 0x1b:
			b.WriteRune('␛')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func needsClean(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || (c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f) {
			return true
		}
	}
	return false
}

// cleanJSON applies clean to every string in a JSON document. JSON can only
// carry controls as escapes (\u00XX, \r, \b, \f) or as raw DEL/C1 bytes, so
// documents without them are returned untouched.
func cleanJSON(raw json.RawMessage) json.RawMessage {
	if !bytes.Contains(raw, []byte(`\u00`)) && !bytes.Contains(raw, []byte(`\r`)) && !bytes.Contains(raw, []byte(`\b`)) &&
		!bytes.Contains(raw, []byte(`\f`)) && !needsClean(string(raw)) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	out, err := json.Marshal(cleanValue(v))
	if err != nil {
		return raw
	}
	return out
}

func cleanValue(v any) any {
	switch x := v.(type) {
	case string:
		return clean(x)
	case []any:
		for i := range x {
			x[i] = cleanValue(x[i])
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[clean(k)] = cleanValue(val)
		}
		return out
	}
	return v
}
