// Package config reads key=value settings.
package config

import (
	"bufio"
	"strconv"
	"strings"
)

// Settings are the client's options.
type Settings struct {
	Endpoint   string
	RetryLimit int
}

// Parse reads key=value lines; unknown keys are ignored. RetryLimit
// defaults to 3.
func Parse(text string) Settings {
	s := Settings{RetryLimit: 3}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "endpoint":
			s.Endpoint = strings.TrimSpace(v)
		case "retry_limit":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				s.RetryLimit = n
			}
		}
	}
	return s
}
