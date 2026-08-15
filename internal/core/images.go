package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func extractImages(value any) (any, []provider.Image) {
	images := []provider.Image{}
	return scrubImages(value, &images), images
}

func scrubImages(value any, images *[]provider.Image) any {
	switch x := value.(type) {
	case map[string]any:
		if fmt.Sprint(x["type"]) == "image" {
			image := provider.Image{}
			image.Data, _ = x["data"].(string)
			image.MediaType, _ = x["mimeType"].(string)
			if image.MediaType == "" {
				image.MediaType, _ = x["mime_type"].(string)
			}
			if u, ok := x["url"].(string); ok {
				image.URL = u
			}
			if image.Data != "" || image.URL != "" {
				*images = append(*images, image)
				return map[string]any{"type": "image", "media_type": image.MediaType, "attached": true}
			}
		}
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = scrubImages(v, images)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = scrubImages(v, images)
		}
		return out
	default:
		return value
	}
}

func messagesHaveImages(messages []store.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		var message provider.Message
		if json.Unmarshal([]byte(messages[i].ContentJSON), &message) == nil && len(message.Images) > 0 {
			return true
		}
		if strings.EqualFold(messages[i].Role, "assistant") {
			break
		}
	}
	return false
}
