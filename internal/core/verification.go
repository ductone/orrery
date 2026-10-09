package core

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ductone/orrey/internal/review"
	"mvdan.cc/sh/v3/syntax"
)

// verificationCommands are commands recognised, without judgement, as checking
// a change: builds, test runners, type checkers, linters, and format checks.
// Each entry is a command name followed by the arguments it needs. A plain
// argument must match the next positional argument by prefix ("make lint"
// matches "make lint/md"); a flag must appear anywhere. A successful command
// outside this list does not count: recognition is mechanical, with no judge.
var verificationCommands = parseVerificationCommands(
	// Build systems and task runners.
	"go test", "go vet", "go build",
	"cargo test", "cargo check", "cargo clippy", "cargo build",
	"bazel test", "bazel build", "bazelisk test", "bazelisk build",
	"make test", "make build", "make lint", "make typecheck", "make check", "make ci", "make vet",
	"npm test", "npm run test", "npm run build", "npm run lint", "npm run typecheck", "npm run check",
	"pnpm test", "pnpm build", "pnpm lint", "pnpm typecheck", "pnpm check",
	"pnpm run test", "pnpm run build", "pnpm run lint", "pnpm run typecheck", "pnpm run check",
	"yarn test", "yarn build", "yarn lint", "yarn typecheck", "yarn check",
	"just test", "just lint", "just check", "task test", "task lint",
	"gradle test", "gradle build", "gradlew test", "gradlew build", "gradlew check",
	"mvn test", "mvn verify", "dotnet test", "dotnet build",
	"swift test", "swift build", "mix test", "ctest", "tox", "nox",
	"deno test", "deno check", "deno lint", "bun test",
	// Test runners, type checkers, and linters invoked directly.
	"pytest", "rspec", "phpunit", "jest", "vitest", "mocha", "playwright test",
	"tsc", "vue-tsc", "mypy", "pyright", "ruff", "flake8", "pylint",
	"eslint", "stylelint", "golangci-lint", "staticcheck", "shellcheck", "hadolint", "actionlint",
	"buf lint", "buf build", "buf breaking",
	"terraform validate", "tflint", "rubocop", "clang-tidy", "swiftlint", "ktlint", "detekt",
)

// formatCheckCommands check formatting or document style, not behaviour. They
// verify prose and configuration, but a code change needs a real check.
var formatCheckCommands = parseVerificationCommands(
	"cargo fmt --check", "gofmt -l", "prettier --check", "prettier -c", "black --check",
	"markdownlint", "markdownlint-cli2", "yamllint", "taplo check", "taplo fmt --check",
)

type verificationPattern struct {
	name       string
	positional []string
	flags      []string
}

func parseVerificationCommands(entries ...string) []verificationPattern {
	out := make([]verificationPattern, 0, len(entries))
	for _, e := range entries {
		words := strings.Fields(e)
		p := verificationPattern{name: words[0]}
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-") {
				p.flags = append(p.flags, w)
			} else {
				p.positional = append(p.positional, w)
			}
		}
		out = append(out, p)
	}
	return out
}

// commandRunners run the command named after their own arguments.
var commandRunners = map[string][]string{
	"npx": nil, "bunx": nil, "pnpx": nil, "time": nil, "nice": nil, "env": nil, "command": nil,
	"timeout": nil, "xargs": nil, "sudo": nil,
}

// runnerValueFlags are runner flags that take a separate value.
var runnerValueFlags = map[string][]string{
	"sudo": {"-u", "-g", "-C", "-D", "-h"}, "env": {"-u", "-C", "-S"}, "timeout": {"-s", "-k"},
	"nice": {"-n"}, "xargs": {"-I", "-n", "-P", "-L", "-s", "-d", "-E", "-a"}, "npx": {"-p", "--package"},
}

// Kinds of recognised check.
const (
	notACheck   = ""
	formatCheck = "format"
	fullCheck   = "check"
)

// isVerificationCommand reports whether a command line runs a recognised
// check of either kind.
func isVerificationCommand(command string) bool { return verificationKind(command) != notACheck }

// verificationKind classifies a command line by the strongest recognised
// check among its simple commands. Commands are parsed, so a file name that
// merely contains a tool's name (cat tsconfig.json) does not count.
func verificationKind(command string) string {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return notACheck
	}
	kind := notACheck
	syntax.Walk(file, func(node syntax.Node) bool {
		if kind == fullCheck {
			return false
		}
		if call, ok := node.(*syntax.CallExpr); ok {
			var words []string
			for _, w := range call.Args {
				if v, ok := shellLiteral(w); ok {
					words = append(words, v)
				} else {
					words = append(words, "")
				}
			}
			words = unwrapRunner(words)
			switch {
			case matchesVerification(words, verificationCommands):
				kind = fullCheck
			case matchesVerification(words, formatCheckCommands):
				kind = formatCheck
			}
		}
		return kind != fullCheck
	})
	return kind
}

// unwrapRunner strips package runners and wrappers: npx eslint, pnpm exec
// tsc, yarn dlx prettier, bundle exec rspec, python -m pytest, uv run mypy.
func unwrapRunner(words []string) []string {
	for len(words) > 0 {
		name := filepath.Base(words[0])
		rest := words[1:]
		switch {
		case name == "pnpm" && len(rest) > 0 && (rest[0] == "exec" || rest[0] == "dlx"),
			name == "yarn" && len(rest) > 0 && (rest[0] == "exec" || rest[0] == "dlx"),
			name == "bundle" && len(rest) > 0 && rest[0] == "exec",
			(name == "uv" || name == "poetry" || name == "pipenv" || name == "hatch" || name == "pdm") && len(rest) > 0 && rest[0] == "run":
			words = rest[1:]
		case strings.HasPrefix(name, "python") && len(rest) > 1 && rest[0] == "-m":
			words = rest[1:]
		case name == "go" && len(rest) > 1 && rest[0] == "tool":
			words = rest[1:]
		default:
			if _, ok := commandRunners[name]; !ok {
				return words
			}
			words = runnerCommand(name, rest)
		}
	}
	return words
}

// runnerCommand skips a runner's own flags (and the values some take),
// assignments, and a timeout duration, returning the command it runs.
func runnerCommand(name string, args []string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains(runnerValueFlags[name], a):
			i++
		case strings.HasPrefix(a, "-") || strings.Contains(a, "=") || (name == "timeout" && i == 0):
		default:
			return args[i:]
		}
	}
	return nil
}

func matchesVerification(words []string, patterns []verificationPattern) bool {
	if len(words) == 0 {
		return false
	}
	name := filepath.Base(words[0])
	var positional, flags []string
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "-") {
			flags = append(flags, strings.SplitN(w, "=", 2)[0])
		} else {
			positional = append(positional, w)
		}
	}
	for _, p := range patterns {
		if p.name != name || len(positional) < len(p.positional) {
			continue
		}
		ok := true
		for i, want := range p.positional {
			if !strings.HasPrefix(positional[i], want) {
				ok = false
				break
			}
		}
		for _, f := range p.flags {
			if !slices.Contains(flags, f) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// shellLiteral returns a word's value when it contains no expansions.
func shellLiteral(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// proseExtensions are documents whose changes no command can meaningfully
// verify beyond formatting.
var proseExtensions = []string{".md", ".mdx", ".markdown", ".rst", ".txt", ".adoc", ".asciidoc", ".org", ".tex"}

// changedPaths is what this run changed: the workspace delta when knowable,
// plus every file edited through the edit tool.
func (e *Engine) changedPaths(ctx context.Context, sid, root string, progress *progressTracker) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if rel, err := filepath.Rel(root, p); err == nil && filepath.IsAbs(p) && !strings.HasPrefix(rel, "..") {
			p = rel
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if paths, _, ok := e.runChanges(ctx, sid, root); ok {
		for _, p := range paths {
			add(p)
		}
	}
	for p := range progress.editedPaths {
		add(p)
	}
	slices.Sort(out)
	return out
}

// syncWorkspaceChanges makes verification and review follow the workspace,
// including changes made through exec rather than the edit tool.
func (e *Engine) syncWorkspaceChanges(ctx context.Context, sid, root string, progress *progressTracker) {
	paths, current, ok := e.runChanges(ctx, sid, root)
	if !ok {
		return
	}
	changed := make(map[string]string, len(paths))
	for _, path := range paths {
		changed[path] = current[path].hash
	}
	hash := fingerprint("workspace", changed)
	if hash == progress.workspaceHash {
		return
	}
	previous := progress.workspaceHash
	progress.workspaceHash = hash
	if len(paths) == 0 && previous == "" {
		return
	}
	progress.edited = true
	progress.turnEdited = true
	progress.turnProgress = true
	progress.verified = false
	progress.reviewed = false
	progress.checksSinceEdit = nil
	progress.verificationAdvised = false
	progress.formatVerified = false
	progress.fixPending = false
	if progress.editedPaths == nil {
		progress.editedPaths = map[string]bool{}
	}
	for _, path := range paths {
		progress.editedPaths[path] = true
	}
}

// needsVerification reports whether any changed file is one a command could
// verify: anything but prose documents and assets.
func needsVerification(paths []string) bool {
	if len(paths) == 0 {
		return true
	}
	for _, p := range paths {
		f := review.File{Path: p}
		review.Classify(&f)
		if f.Class == review.Asset {
			continue
		}
		if f.Class == review.Other && slices.Contains(proseExtensions, strings.ToLower(filepath.Ext(p))) {
			continue
		}
		return true
	}
	return false
}

// changesCode reports whether any changed file is code, which a formatting
// check cannot verify.
func changesCode(paths []string) bool {
	for _, p := range paths {
		f := review.File{Path: p}
		review.Classify(&f)
		if f.Class == review.Code {
			return true
		}
	}
	return false
}

// verificationSatisfied reports whether an edited run may complete without a
// recognised verification command: when it changed only prose and assets, or
// when a format check covers configuration changes. Verification is advice, not
// a gate, so a false result only produces a note.
func (e *Engine) verificationSatisfied(ctx context.Context, sid, root string, progress *progressTracker, emit EmitFunc) bool {
	changed := e.changedPaths(ctx, sid, root, progress)
	if !needsVerification(changed) {
		e.emit(ctx, sid, "verification.accepted", map[string]any{"reason": "only prose and asset files changed", "changed": changed}, emit)
		return true
	}
	if progress.formatVerified && !changesCode(changed) {
		e.emit(ctx, sid, "verification.accepted", map[string]any{"reason": "a format or style check covers configuration changes", "changed": changed}, emit)
		return true
	}
	return false
}

// verificationAdvice is the one note a run sees for an unverified change, and it
// is advice: completion is not refused. It asks for a real check and says a run
// may finish without one when nothing applies, so a model is not pushed to
// invent a build target or wrapper script to satisfy a gate.

// shouldAdviseVerification reports whether an unverified change still has
// something a command could check. A run that made no edit, committed its
// change, or touched only prose and assets gets no note. Outside git the
// edit-tool paths stand in for the workspace delta, which is not knowable.
func (e *Engine) shouldAdviseVerification(ctx context.Context, sid, root string, progress *progressTracker) bool {
	if paths, _, ok := e.runChanges(ctx, sid, root); ok {
		return len(paths) > 0 && needsVerification(paths)
	}
	return needsVerification(e.changedPaths(ctx, sid, root, progress))
}

const verificationAdvice = "Verification advice: you changed files a command could check, but no successful command checked them. Consider running the relevant test, build, type check, or linter for the files you changed and reporting what you ran. If no existing check applies to these changes, say so in your final result. Do not add or change build targets, scripts, CI, or configuration to create a check. You may finish without verification when none applies."
