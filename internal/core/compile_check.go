package core

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// goCompileCheck runs `go build ./...` in the given workspace directory with a
// 60-second timeout. It returns the combined output, whether the build succeeded,
// and whether the check actually ran (false when Go is unavailable or the
// infrastructure itself failed, in which case the caller should silently skip).
func goCompileCheck(ctx context.Context, workspacePath string) (output string, ok bool, ran bool) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", true, false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, "go", "build", "./...")
	cmd.Dir = workspacePath
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	combined := buf.String()
	if err == nil {
		return "", true, true
	}
	// If the context was cancelled (timeout or parent cancel), treat as infra failure.
	if checkCtx.Err() != nil {
		return "", true, false
	}
	// Truncate to ~40 lines to keep the annotation compact.
	lines := strings.Split(combined, "\n")
	if len(lines) > 40 {
		combined = strings.Join(lines[:40], "\n") + "\n... (truncated)"
	}
	return combined, false, true
}
