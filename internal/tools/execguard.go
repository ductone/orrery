package tools

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The exec guard steers source changes to the edit tool, where they are
// anchored, reviewable, and measured. It is not a security boundary: Orrery
// runs inside an isolated workspace and is not an approval system. A missed
// mutation costs little, while a false rejection costs the model a turn, so
// the guard parses the command and rejects only writes it can see, to paths
// inside the workspace. A command that does not parse is allowed.

// harmlessTargets are write targets that never touch a file.
var harmlessTargets = []string{"/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty"}

// wrappers run the command that follows their own flags.
var wrappers = []string{"sudo", "doas", "env", "command", "builtin", "exec", "nice", "nohup", "time", "timeout", "xargs", "stdbuf", "ionice"}

// interpreters run inline code whose file writes the guard looks for.
var interpreters = []string{"python", "python2", "python3", "node", "deno", "bun", "ruby", "perl", "php"}

// inlineWrites are file-writing calls recognised inside inline scripts.
// Matched case-insensitively by method name, so os.WriteFile, ioutil.WriteFile,
// and require("fs").writeFileSync are all caught.
var inlineWrites = []string{".write_text(", ".write_bytes(", "writefilesync(", "writefile(", "os.create(", "file.write("}

// shells run a script given with -c, which the guard parses in turn.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh"}

type execGuard struct {
	root string
	cwd  string
	// redirects maps each call to its statement's redirects; the parser hangs
	// them on the statement, and Walk visits the statement first.
	redirects map[*syntax.CallExpr][]*syntax.Redirect
	depth     int
}

// sourceMutation reports why a command would modify files in the workspace,
// or "" when it would not (or cannot be parsed).
func sourceMutation(command, root string) string {
	root = cleanRoot(root)
	return (&execGuard{root: root, cwd: root}).script(command)
}

func (g *execGuard) script(command string) string {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return ""
	}
	if g.redirects == nil {
		g.redirects = map[*syntax.CallExpr][]*syntax.Redirect{}
	}
	reason := ""
	// Walk visits nodes in source order, so a cd applies to the commands after
	// it. Subshell scoping is ignored; that only matters for commands that cd
	// out of the workspace inside a subshell and write relative paths after it.
	syntax.Walk(file, func(node syntax.Node) bool {
		if reason != "" {
			return false
		}
		switch n := node.(type) {
		case *syntax.Stmt:
			if call, ok := n.Cmd.(*syntax.CallExpr); ok {
				g.redirects[call] = n.Redirs
			}
		case *syntax.Redirect:
			reason = g.redirect(n)
		case *syntax.CallExpr:
			reason = g.call(n)
		}
		return reason == ""
	})
	return reason
}

func cleanRoot(root string) string {
	if root == "" {
		return ""
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
}

func (g *execGuard) redirect(r *syntax.Redirect) string {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.RdrAllClob, syntax.AppAll, syntax.AppAllClob, syntax.RdrInOut:
	default:
		// Input redirects, here-documents, and descriptor duplication (2>&1).
		return ""
	}
	target, ok := literal(r.Word)
	if !ok {
		return "output redirect to a computed path " + quoteWord(r.Word)
	}
	if g.inWorkspace(target) {
		return fmt.Sprintf("output redirect %s to %q", r.Op, target)
	}
	return ""
}

func (g *execGuard) call(c *syntax.CallExpr) string {
	args := make([]string, 0, len(c.Args))
	for _, w := range c.Args {
		v, ok := literal(w)
		if !ok {
			// A computed argument is kept as a placeholder so positions hold;
			// it can never match a command name or flag.
			v = "\x00" + quoteWord(w)
		}
		args = append(args, v)
	}
	return g.command(args, c)
}

// command checks one invocation. c is the enclosing call, used to reach its
// here-document bodies for interpreters; it is nil for commands found inside
// another command's arguments (xargs, find -exec).
func (g *execGuard) command(args []string, c *syntax.CallExpr) string {
	if len(args) == 0 {
		return ""
	}
	name := filepath.Base(args[0])
	rest := args[1:]
	switch {
	case name == "cd":
		g.cd(rest)
		return ""
	case slices.Contains(wrappers, name):
		return g.command(unwrap(name, rest), nil)
	case name == "find":
		return g.find(rest)
	case slices.Contains(shells, name):
		return g.shell(rest)
	case name == "sed":
		if sedInPlace(rest) && g.anyWorkspaceFile(sedFiles(rest)) {
			return "sed -i edits files in place"
		}
		return ""
	case name == "perl":
		if perlInPlace(rest) && g.anyWorkspaceFile(perlFiles(rest)) {
			return "perl -i edits files in place"
		}
	case name == "touch":
		if target := g.firstWorkspaceOperand(rest); target != "" {
			return fmt.Sprintf("touch creates or modifies %q", target)
		}
		return ""
	case name == "tee":
		if target := g.firstWorkspaceOperand(rest); target != "" {
			return fmt.Sprintf("tee writes %q", target)
		}
		return ""
	case name == "gofmt" || name == "goimports":
		if slices.Contains(rest, "-w") && g.anyWorkspaceFile(operands(rest)) {
			return name + " -w rewrites files"
		}
		return ""
	case name == "go":
		if len(rest) > 0 && rest[0] == "fmt" {
			return "go fmt rewrites files"
		}
		return ""
	}
	if isInterpreter(name) {
		return g.inlineScript(name, rest, c)
	}
	return ""
}

func (g *execGuard) cd(args []string) {
	for _, a := range args {
		if a == "--" || (strings.HasPrefix(a, "-") && a != "-") {
			continue
		}
		if strings.HasPrefix(a, "\x00") || a == "-" || a == "~" || strings.HasPrefix(a, "~/") {
			// Unknown destination: assume it may be inside the workspace.
			g.cwd = ""
			return
		}
		g.cwd = g.resolve(a)
		return
	}
	g.cwd = ""
}

// unwrap returns the command a wrapper runs: the first argument after the
// wrapper's own flags, assignments, and (for timeout) duration.
func unwrap(name string, args []string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:]
		case strings.HasPrefix(a, "-"):
			// Flags that take a separate value.
			if (name == "xargs" && slices.Contains([]string{"-I", "-n", "-P", "-L", "-s", "-d", "-E", "-a"}, a)) ||
				(name == "sudo" && slices.Contains([]string{"-u", "-g", "-C", "-D"}, a)) ||
				(name == "nice" && a == "-n") || (name == "timeout" && (a == "-s" || a == "-k")) ||
				(name == "env" && (a == "-u" || a == "-C")) || (name == "ionice" && (a == "-c" || a == "-n")) {
				i++
			}
		case name == "env" && strings.Contains(a, "="):
		case name == "timeout" && i == firstNonFlag(args):
		default:
			return args[i:]
		}
	}
	return nil
}

func firstNonFlag(args []string) int {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return i
		}
	}
	return -1
}

// find mutates through -exec/-execdir/-ok running a mutating command.
// -delete removes files but the guard only covers edits, as it always has.
// The {} placeholder stands for files under find's starting points, so it is
// replaced by them: find /tmp -exec gofmt -w {} + does not touch the workspace.
func (g *execGuard) find(args []string) string {
	var starts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		starts = append(starts, a)
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
			end := i + 1
			for end < len(args) && args[end] != ";" && args[end] != `\;` && args[end] != "+" {
				end++
			}
			var sub []string
			for _, a := range args[i+1 : end] {
				if a == "{}" {
					sub = append(sub, starts...)
				} else {
					sub = append(sub, a)
				}
			}
			if reason := g.command(sub, nil); reason != "" {
				return reason
			}
			i = end
		}
	}
	return ""
}

func sedInPlace(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return false
		case a == "--in-place" || strings.HasPrefix(a, "--in-place="):
			return true
		case strings.HasPrefix(a, "--"):
		case a == "-e" || a == "-f" || a == "-l":
			// The next argument is a script, script file, or line length.
			i++
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for _, ch := range a[1:] {
				if ch == 'i' {
					return true
				}
				// e and f take the rest of the cluster (or the next argument) as a value.
				if ch == 'e' || ch == 'f' || ch == 'l' {
					break
				}
			}
		}
	}
	return false
}

// sedFiles returns sed's file operands: the positionals after the script,
// which is the first positional unless -e or -f supplied it.
func sedFiles(args []string) []string {
	var positional []string
	scripted := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
			scripted = true
			i++
		case strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file="):
			scripted = true
		case a == "-l":
			i++
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A bare -i may be followed by BSD sed's backup suffix argument.
			if a == "-i" && i+1 < len(args) && args[i+1] == "" {
				i++
			}
			for _, ch := range a[1:] {
				if ch == 'e' || ch == 'f' {
					scripted = true
					if strings.HasSuffix(a, string(ch)) {
						i++
					}
					break
				}
			}
		default:
			positional = append(positional, a)
		}
	}
	if !scripted && len(positional) > 0 {
		positional = positional[1:]
	}
	return positional
}

// perlFiles returns perl's file operands: the positionals after the program,
// which is the first positional unless -e supplied it.
func perlFiles(args []string) []string {
	var positional []string
	scripted := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-") && len(a) > 1 && len(positional) == 0:
			for j, ch := range a[1:] {
				if ch == 'e' || ch == 'E' {
					scripted = true
					if j == len(a)-2 {
						i++
					}
					break
				}
				if strings.ContainsRune("MmIxCdDlF0", ch) {
					break
				}
			}
		default:
			positional = append(positional, a)
		}
	}
	if !scripted && len(positional) > 0 {
		positional = positional[1:]
	}
	return positional
}

func operands(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
		}
	}
	return out
}

// anyWorkspaceFile reports whether an in-place editor's files include one in
// the workspace. No files at all means input from stdin or a guess the guard
// cannot make, which is treated as a workspace edit.
func (g *execGuard) anyWorkspaceFile(files []string) bool {
	if len(files) == 0 {
		return true
	}
	for _, f := range files {
		if strings.HasPrefix(f, "\x00") || g.inWorkspace(f) {
			return true
		}
	}
	return false
}

func perlInPlace(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--" || !strings.HasPrefix(a, "-"):
			// Perl stops at the program file or the first non-switch argument.
			return false
		case strings.HasPrefix(a, "--"):
			continue
		}
		for _, ch := range a[1:] {
			if ch == 'i' {
				return true
			}
			// Switches that take the rest of the cluster as a value.
			if strings.ContainsRune("eEMmIxCdDlF0", ch) {
				break
			}
		}
	}
	return false
}

func (g *execGuard) firstWorkspaceOperand(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		if strings.HasPrefix(a, "\x00") {
			return strings.TrimPrefix(a, "\x00")
		}
		if g.inWorkspace(a) {
			return a
		}
	}
	return ""
}

func isInterpreter(name string) bool {
	if slices.Contains(interpreters, name) {
		return true
	}
	// python3.12 and the like.
	return strings.HasPrefix(name, "python") && strings.Trim(strings.TrimPrefix(name, "python"), "0123456789.") == ""
}

// inlineScript looks for file-writing calls in code passed on the command line
// or in a here-document. A script file argument is not read: its writes are
// out of the guard's sight, as they always have been.
func (g *execGuard) inlineScript(name string, args []string, c *syntax.CallExpr) string {
	var code []string
	for _, a := range args {
		code = append(code, strings.TrimPrefix(a, "\x00"))
	}
	if c != nil {
		for _, r := range g.redirects[c] {
			if r.Hdoc != nil {
				code = append(code, wordText(r.Hdoc))
			}
		}
	}
	text := strings.ToLower(strings.Join(code, "\n"))
	for _, call := range inlineWrites {
		if strings.Contains(text, call) {
			return fmt.Sprintf("inline %s script calls %s", name, strings.TrimSuffix(call, "("))
		}
	}
	return ""
}

// shell checks the script passed to sh -c. Nesting is bounded so a
// pathological command cannot recurse without limit.
func (g *execGuard) shell(args []string) string {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && g.depth < 4 {
			nested := &execGuard{root: g.root, cwd: g.cwd, depth: g.depth + 1}
			return nested.script(strings.TrimPrefix(args[i+1], "\x00"))
		}
	}
	return ""
}

func (g *execGuard) resolve(p string) string {
	if !filepath.IsAbs(p) {
		base := g.cwd
		if base == "" {
			return ""
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(resolved, filepath.Base(p))
	}
	return p
}

// inWorkspace reports whether a write target is a workspace file the guard
// protects. Unknown locations count as inside, except harmless devices.
// Orrery's own .orrery directory is scratch space, not source.
func (g *execGuard) inWorkspace(target string) bool {
	if slices.Contains(harmlessTargets, target) || strings.HasPrefix(target, "/dev/fd/") {
		return false
	}
	if g.root == "" {
		return true
	}
	abs := g.resolve(target)
	if abs == "" {
		return true
	}
	rel, err := filepath.Rel(g.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return rel != ".orrery" && !strings.HasPrefix(rel, ".orrery/")
}

// literal returns a word's value when it contains no expansions.
func literal(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
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

// wordText renders a word, expansions included, for messages and scripts.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	_ = syntax.NewPrinter().Print(&b, w)
	return b.String()
}

func quoteWord(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	return fmt.Sprintf("%q", wordText(w))
}
