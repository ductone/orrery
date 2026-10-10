// Package users validates user records against schema/user.json.
package users

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed schema/user.json
var schemaJSON []byte

type schema struct {
	Required   []string          `json:"required"`
	Properties map[string]string `json:"properties"`
}

// Validate reports the first required field missing from u.
func Validate(u map[string]string) error {
	var s schema
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return err
	}
	for _, f := range s.Required {
		if u[f] == "" {
			return fmt.Errorf("missing required field %q", f)
		}
	}
	return nil
}
