package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The exec guard rejects commands that destroy work the agent cannot recover:
// discarded uncommitted changes, a force-push, or a recursive removal of the
// workspace, its parents, or paths outside it. It is not a security boundary:
// Orrery runs inside an isolated workspace and is not an approval system.
// Writes inside the workspace (formatters, redirection, generators, sed -i)
// are allowed; the final diff is reviewed independently whatever wrote it. A
// command that does not parse is allowed.

// wrappers run the command that follows their own flags.
var wrappers = []string{"sudo", "doas", "env", "command", "builtin", "exec", "nice", "nohup", "time", "timeout", "xargs", "stdbuf", "ionice"}

// shells run a script given with -c, which the guard parses in turn.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh"}

type execGuard struct {
	root  string
	cwd   string
	depth int
}

// destructiveCommand reports why a command would discard work that cannot be
// recovered, or "" when it would not (or cannot be parsed).
func destructiveCommand(command, root string) string {
	root = cleanRoot(root)
	return (&execGuard{root: root, cwd: root}).script(command)
}

func (g *execGuard) script(command string) string {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return ""
	}
	reason := ""
	// Walk visits nodes in source order, so a cd applies to the commands after
	// it. Subshell scoping is ignored; that only matters for commands that cd
	// inside a subshell and act on relative paths after it.
	syntax.Walk(file, func(node syntax.Node) bool {
		if reason != "" {
			return false
		}
		if c, ok := node.(*syntax.CallExpr); ok {
			reason = g.call(c)
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
	return g.command(args)
}

// command checks one invocation. Commands found inside another command's
// arguments (xargs, find -exec) arrive here with no enclosing call.
func (g *execGuard) command(args []string) string {
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
		return g.command(unwrap(name, rest))
	case name == "find":
		return g.find(rest)
	case slices.Contains(shells, name):
		return g.shell(rest)
	case name == "git":
		return g.git(rest)
	case name == "rm":
		return g.rm(rest)
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

// find runs commands through -exec/-execdir/-ok. The {} placeholder stands
// for files under find's starting points, so it is replaced by them.
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
			if reason := g.command(sub); reason != "" {
				return reason
			}
			i = end
		}
	}
	return ""
}

// git rejects commands that discard work that cannot be recovered: a hard
// reset, a forced clean, a checkout or restore that throws away working-tree
// changes, dropping stashes, and a force-push. --force-with-lease is allowed.
func (g *execGuard) git(args []string) string {
	flags, pos := gitArgs(args)
	if len(pos) == 0 {
		return ""
	}
	switch pos[0] {
	case "reset":
		if slices.Contains(flags, "--hard") {
			return "git reset --hard discards uncommitted changes"
		}
	case "clean":
		for _, f := range flags {
			if f == "--force" || f == "-f" || f == "-ff" || clusteredFlag(f, "f") {
				return "git clean -f deletes untracked files"
			}
		}
	case "checkout":
		if gitCheckoutDiscards(flags, pos[1:]) {
			return "git checkout -- discards working-tree changes"
		}
	case "restore":
		if !gitRestoreStagedOnly(flags) {
			return "git restore discards working-tree changes"
		}
	case "stash":
		if len(pos) > 1 && (pos[1] == "drop" || pos[1] == "clear") {
			return "git stash " + pos[1] + " discards stashed changes"
		}
	case "push":
		for _, f := range flags {
			if f == "--force" || f == "-f" || strings.HasPrefix(f, "--force=") || clusteredFlag(f, "f") {
				return "git push --force overwrites remote history"
			}
		}
		// A leading + on a refspec (git push origin +main) is a force-push.
		// --force-with-lease is a flag, so it never reaches here.
		for _, p := range pos[1:] {
			if strings.HasPrefix(p, "+") {
				return "git push --force overwrites remote history"
			}
		}
	}
	return ""
}

// gitArgs splits git's global options from the subcommand and its arguments.
func gitArgs(args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i:]...)
			return flags, positional
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
			// Options that take a separate value, global and per-subcommand.
			if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" || a == "-m" || a == "--message" || a == "-b" || a == "-B" {
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	return flags, positional
}

// gitCheckoutDiscards reports a checkout that throws away working-tree
// changes: `checkout -- <paths>` or `checkout .`. Switching branches
// (checkout <branch>, checkout -b) does not.
func gitCheckoutDiscards(flags, rest []string) bool {
	if slices.Contains(flags, "-b") || slices.Contains(flags, "-B") || slices.Contains(rest, "-b") || slices.Contains(rest, "-B") {
		return false
	}
	if i := slices.Index(rest, "--"); i >= 0 {
		return i+1 < len(rest)
	}
	for _, p := range rest {
		if p == "." || p == "./" {
			return true
		}
	}
	return false
}

// gitRestoreStagedOnly reports a restore limited to the index. A restore
// without --staged rewrites the working tree and discards its changes.
func gitRestoreStagedOnly(flags []string) bool {
	staged, worktree := false, false
	for _, f := range flags {
		switch f {
		case "--staged", "--staged=true":
			staged = true
		case "--worktree", "--worktree=true":
			worktree = true
		}
	}
	return staged && !worktree
}

// clusteredFlag reports a short-option cluster carrying ch, such as -fd.
func clusteredFlag(f, ch string) bool {
	return strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.Contains(f, ch)
}

// rm rejects a recursive removal whose operand is the workspace root, a
// parent of it, /, ~, or a path outside the workspace. The OS temp dir and
// /tmp are scratch space and allowed.
func (g *execGuard) rm(args []string) string {
	recursive := false
	var operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		case a == "--recursive":
			recursive = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			if strings.ContainsAny(a, "rR") {
				recursive = true
			}
		default:
			operands = append(operands, a)
		}
	}
	if !recursive {
		return ""
	}
	for _, op := range operands {
		if g.rmDestroys(op) {
			return fmt.Sprintf("rm -r %s removes files that cannot be recovered", op)
		}
	}
	return ""
}

// rmDestroys reports whether a recursive rm operand would discard
// irrecoverable work: the workspace root, a parent of it, /, ~, or anything
// outside the workspace except the OS temp dir and /tmp.
func (g *execGuard) rmDestroys(op string) bool {
	if strings.HasPrefix(op, "\x00") {
		// A computed operand could be anything, including the workspace.
		return true
	}
	if op == "/" || op == "~" || strings.HasPrefix(op, "~/") {
		return true
	}
	if g.root == "" {
		return !rmScratch(op)
	}
	abs := g.resolve(op)
	if abs == "" {
		return true
	}
	// Scratch paths are checked before resolution: the OS temp dir is often a
	// symlink, and resolving it would hide that the operand is disposable.
	if rmScratch(op) || rmScratch(abs) {
		return false
	}
	rel, err := filepath.Rel(g.root, abs)
	if err != nil {
		return true
	}
	// The workspace root itself, or a parent of it. A path inside the
	// workspace is the agent's own work and is allowed.
	return rel == "." || rel == ".." || strings.HasPrefix(rel, "../")
}

// rmScratch reports paths that hold nothing but disposable files: /tmp and
// the OS temp dir, plus anything nested under them.
func rmScratch(p string) bool {
	clean := filepath.Clean(p)
	for _, tmp := range []string{"/tmp", filepath.Clean(os.TempDir())} {
		if tmp == "" || tmp == "." {
			continue
		}
		if clean == tmp || strings.HasPrefix(clean, tmp+"/") || strings.HasPrefix(clean, tmp+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// shell checks the script passed to sh -c. Nesting is bounded so a
// pathological command cannot recurse without limit.
func (g *execGuard) shell(args []string) string {
	// A shell's own flags precede -c, and -c takes the script as its value.
	for i := 0; i < len(args) && g.depth < 4; i++ {
		a := args[i]
		switch {
		case a == "-c" && i+1 < len(args):
			nested := &execGuard{root: g.root, cwd: g.cwd, depth: g.depth + 1}
			return nested.script(strings.TrimPrefix(args[i+1], "\x00"))
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "="):
			// An assignment (sh VAR=x -c ...) is not the script.
		default:
			// The command name: this shell runs a file, not inline code.
			return ""
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
