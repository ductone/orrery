// Package settings loads a JSON settings file.
package settings

import (
	"encoding/json"
	"os"
)

// Settings is the on-disk configuration.
type Settings struct {
	Name    string `json:"name"`
	Workers int    `json:"workers"`
}

// Load reads and decodes the settings file at path.
func Load(path string) (Settings, error) {
	var s Settings
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &s)
	return s, nil
}
