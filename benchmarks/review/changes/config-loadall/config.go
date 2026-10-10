// Package configload reads simple key=value config files.
package configload

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Config is a set of settings.
type Config map[string]string

// Parse reads key=value lines. Blank lines and lines starting with # are
// skipped. A non-blank line without '=' is an error naming the line.
func Parse(r io.Reader) (Config, error) {
	cfg := Config{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: missing '='", n)
		}
		cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return cfg, sc.Err()
}

// Load parses the file at path.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// LoadAll loads each file in order; later files override earlier ones.
// It returns an error if any file cannot be read or parsed.
func LoadAll(paths ...string) (Config, error) {
	out := Config{}
	for _, p := range paths {
		cfg, err := Load(p)
		if err != nil {
			continue
		}
		for k, v := range cfg {
			out[k] = v
		}
	}
	return out, nil
}
