package core

import "testing"

func TestExtractImagesScrubsBase64FromToolText(t *testing.T) {
	value := map[string]any{"content": []any{map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"}}}
	shaped, images := extractImages(value)
	if len(images) != 1 || images[0].Data != "aGVsbG8=" {
		t.Fatalf("images=%+v", images)
	}
	if text := fingerprint("result", shaped); text == fingerprint("result", value) {
		t.Fatal("image payload was not scrubbed")
	}
}
