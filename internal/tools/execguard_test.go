package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardRoot is a stand-in workspace path for commands that name absolute paths.
const guardRoot = "/work/repo"

// realFalsePositives have the shapes of commands the regex guard rejected in
// practice. None of them writes a file.
var realFalsePositives = []string{
	`cd /home/dev/code && ls -la && echo --- && find . -maxdepth 4 -name ".git" 2>/dev/null`,
	`cd /home/dev/code/github.com && ls && echo "--- orrery candidates ---" && find . -maxdepth 3 -iname "*orrery*" -maxdepth 3 2>/dev/null | head -50`,
	`ls -d /home/dev/code/github.com/*/*orrery* 2>/dev/null; echo "---"; ls -d /home/dev/code/github.com/*/*Orrery* 2>/dev/null`,
	`cd /work/repo && cat .orrery/logs/../../.git/HEAD >/dev/null; git status --porcelain && echo "=== STAT ===" && git diff --stat`,
	`cd github.com/ductone/orrery && git status --porcelain && echo --- && find . -iname '*index*' -path '*test*' 2>/dev/null && echo --- && grep -rl "struct" . | head`,
	`cd /work/other && ls -R --  . 2>/dev/null | head -5; find . -path ./.git -prune -o -type f -print | grep -v '^./vendor' | head -40`,
	`cd /work/repo && ls protos/ && echo === && ls protos/c1* 2>/dev/null | head -40`,
	`cd /work/repo && git ls-files | grep -i 'gateway' | grep -E '\.proto$|\.md$' | head -20; echo ===; sed -n '1,160p' protos/gateway.proto`,
	`cd /work/repo && ls docs/rfcs 2>/dev/null | head -20; echo ---; ls docs/rfcs/*.md 2>/dev/null | head -3 | xargs -I{} sh -c 'echo "== {}"; head -40 {}'`,
}

func TestGuardAllowsRealFalsePositives(t *testing.T) {
	for _, cmd := range realFalsePositives {
		if reason := sourceMutation(cmd, guardRoot); reason != "" {
			t.Errorf("rejected %q: %s", cmd, reason)
		}
	}
}

func TestGuardAllowsReadOnlyCommands(t *testing.T) {
	for _, cmd := range []string{
		// Plain reads and listings.
		`ls -la`,
		`cat go.mod`,
		`head -50 main.go && tail -20 main.go`,
		`wc -l **/*.go`,
		`git status`,
		`git diff --stat HEAD~3`,
		`git log --oneline -20 | head`,
		`git show HEAD:README.md`,
		`find . -name '*.go' -newer go.mod`,
		`tree -L 2`,
		// Separators and harmless redirects in every position.
		`echo --- && ls`,
		`echo ---; ls 2>/dev/null`,
		`echo "=== a ===" ; cat a.go 2> /dev/null ; echo "=== b ==="`,
		`echo hi >/dev/null`,
		`echo hi > /dev/null`,
		`echo err >&2`,
		`echo out 1>&2`,
		`go test ./... 2>&1 | tail -30`,
		`go build ./... &>/dev/null && echo ok`,
		`make test &> /dev/null || echo failed`,
		`printf '%s\n' a b >/dev/stdout`,
		`echo warn >/dev/stderr`,
		`echo x > /dev/fd/3`,
		`echo x >/dev/tty`,
		`command -v gofmt >/dev/null 2>&1 && echo present`,
		// Input redirects and here-strings are not writes.
		`wc -l < main.go`,
		`grep -c foo <<< "foo bar"`,
		`cat <<EOF
just text
EOF`,
		`while read -r line; do echo "$line"; done < files.txt`,
		// sed, perl, and formatters without in-place flags.
		`sed -n '1,160p' main.go`,
		`sed -n -e '/func main/,/^}/p' main.go`,
		`sed -e 's/-i/x/' main.go`,
		`sed -E 's/(a)(b)/\2\1/' main.go`,
		`sed --quiet 10p main.go`,
		`perl -ne 'print if /TODO/' main.go`,
		`perl -e 'print "hi\n"'`,
		`perl -Mstrict -e 'print 1'`,
		`gofmt -l .`,
		`gofmt -d main.go`,
		`goimports -l .`,
		`go vet ./...`,
		`go test -run TestFoo ./internal/...`,
		// Searches for the strings the guard looks for in scripts.
		`grep -rn "os.WriteFile(" .`,
		`rg 'write_text\(' --type py`,
		`git grep -n "sed -i"`,
		`grep -rn "touch " scripts/`,
		`echo "use sed -i to edit"`,
		// Wrappers around read-only commands.
		`timeout 30 go test ./...`,
		`env GOFLAGS=-mod=mod go list ./...`,
		`xargs -n1 echo < list.txt`,
		`find . -name '*.go' -exec grep -l TODO {} +`,
		`find . -type f -exec wc -l {} \;`,
		`sh -c 'ls; echo done'`,
		`bash -c "go test ./... 2>&1 | tail"`,
		// Writes outside the workspace, or to Orrery's scratch directory.
		`go test ./... > /tmp/test.log 2>&1`,
		`echo data > /tmp/scratch.txt`,
		`cd /tmp && echo x > notes.txt`,
		`cat > /tmp/check.py <<'EOF'
print("hello")
EOF`,
		`tee /tmp/out.log < input.txt`,
		`touch /tmp/marker`,
		`go test ./... | tee /dev/null`,
		`echo x > .orrery/notes.md`,
		`echo x >> .orrery/logs/run.log`,
		`echo x > /work/repo/.orrery/scratch`,
		`sed -i 's/a/b/' /tmp/copy.go`,
		`sed -i -e 's/a/b/' -e 's/c/d/' /tmp/a /tmp/b`,
		`perl -pi -e 's/a/b/' /tmp/copy.pl`,
		`gofmt -w /tmp/gen.go`,
		`find /tmp/gen -name '*.go' -exec gofmt -w {} +`,
		// Interpreters that read or print.
		`python3 -c 'import json,sys; print(json.load(sys.stdin)["a"])' < data.json`,
		`python -c "print(open('go.mod').read())"`,
		`node -e 'console.log(require("./package.json").version)'`,
		`python3 - <<'EOF'
import pathlib
print(pathlib.Path("go.mod").read_text())
EOF`,
		`python3 scripts/report.py`,
		// Command substitution and subshells that only read.
		`echo "branch: $(git rev-parse --abbrev-ref HEAD)"`,
		`(cd internal && ls)`,
		`for f in *.go; do echo "== $f"; head -5 "$f"; done`,
		`if [ -f go.mod ]; then echo module; fi`,
		// Not valid shell: allowed, since the guard only rejects what it can see.
		`echo "unterminated`,
		``,
	} {
		if reason := sourceMutation(cmd, guardRoot); reason != "" {
			t.Errorf("rejected %q: %s", cmd, reason)
		}
	}
}

func TestGuardRejectsWorkspaceMutations(t *testing.T) {
	for _, tc := range []struct{ cmd, reason string }{
		// Output redirects to workspace files, in every form.
		{`echo x > config.go`, `output redirect > to "config.go"`},
		{`echo x >config.go`, `"config.go"`},
		{`echo x >> CHANGELOG.md`, `output redirect >> to "CHANGELOG.md"`},
		{`printf 'a\n' > notes.txt`, `"notes.txt"`},
		{`cat > main.go <<'EOF'
package main
EOF`, `"main.go"`},
		{`cat <<EOF > internal/x.go
package x
EOF`, `"internal/x.go"`},
		{`echo x >| forced.txt`, `>| to "forced.txt"`},
		{`go test ./... &> test.log`, `"test.log"`},
		{`go test ./... &>> test.log`, `"test.log"`},
		{`go test ./... > out.txt 2>&1`, `"out.txt"`},
		{`ls 2> errors.txt`, `"errors.txt"`},
		{`echo x > "quoted name.go"`, `"quoted name.go"`},
		{`echo x > 'single.go'`, `"single.go"`},
		{`echo x > /work/repo/abs.go`, `"/work/repo/abs.go"`},
		{`echo x > ./rel/../file.go`, `"./rel/../file.go"`},
		{`echo x > "$OUT"`, `computed path`},
		{`echo x > $(mktemp -p .)`, `computed path`},
		{`ls; echo --- > sep.txt`, `"sep.txt"`},
		{`echo a && echo b > b.txt`, `"b.txt"`},
		{`cat a.go | sort > sorted.go`, `"sorted.go"`},
		{`(echo x > sub.go)`, `"sub.go"`},
		{`{ echo a; echo b; } > group.txt`, `"group.txt"`},
		{`for f in a b; do echo $f >> list.txt; done`, `"list.txt"`},
		{`if true; then echo x > cond.go; fi`, `"cond.go"`},
		{`echo "$(echo inner > inner.go)"`, `"inner.go"`},
		{`exec 3> fd.txt`, `"fd.txt"`},
		{`cd /tmp && cd /work/repo && echo x > back.go`, `"back.go"`},
		{`cd internal && echo x > moved.go`, `"moved.go"`},
		{`cd "$DIR" && echo x > unknown.go`, `"unknown.go"`},
		// sed and perl in place.
		{`sed -i 's/a/b/' main.go`, `sed -i`},
		{`sed -i '' 's/a/b/' main.go`, `sed -i`},
		{`sed -i.bak 's/a/b/' main.go`, `sed -i`},
		{`sed -Ei 's/a/b/' main.go`, `sed -i`},
		{`sed -ni 's/a/b/p' main.go`, `sed -i`},
		{`sed --in-place 's/a/b/' main.go`, `sed -i`},
		{`sed --in-place=.orig 's/a/b/' main.go`, `sed -i`},
		{`sed -e 's/a/b/' -i main.go`, `sed -i`},
		{`/usr/bin/sed -i 's/a/b/' main.go`, `sed -i`},
		{`perl -pi -e 's/a/b/' main.go`, `perl -i`},
		{`perl -i.bak -pe 's/a/b/' main.go`, `perl -i`},
		{`perl -pie 's/a/b/' main.go`, `perl -i`},
		{`sed -i 's/a/b/' /tmp/ok.go main.go`, `sed -i`},
		{`sed -i -e 's/a/b/' /work/repo/main.go`, `sed -i`},
		{`sed -i 's/a/b/' "$FILE"`, `sed -i`},
		{`sed -i 's/a/b/'`, `sed -i`},
		{`perl -pi -e 's/a/b/' /tmp/ok main.go`, `perl -i`},
		{`gofmt -w /tmp/ok.go main.go`, `gofmt -w`},
		// Creating and rewriting files.
		{`touch new.go`, `touch creates or modifies "new.go"`},
		{`touch -a -m existing.go`, `"existing.go"`},
		{`touch /tmp/ok internal/new.go`, `"internal/new.go"`},
		{`echo x | tee out.txt`, `tee writes "out.txt"`},
		{`echo x | tee -a log.txt`, `"log.txt"`},
		{`go test ./... | tee /tmp/ok.log report.txt`, `"report.txt"`},
		{`gofmt -w main.go`, `gofmt -w`},
		{`gofmt -l -w .`, `gofmt -w`},
		{`goimports -w .`, `goimports -w`},
		{`go fmt ./...`, `go fmt`},
		// Wrappers and indirection.
		{`sudo sed -i 's/a/b/' main.go`, `sed -i`},
		{`env LC_ALL=C sed -i 's/a/b/' main.go`, `sed -i`},
		{`timeout 10 gofmt -w .`, `gofmt -w`},
		{`nice -n 5 touch x.go`, `"x.go"`},
		{`find . -name '*.go' | xargs sed -i 's/a/b/'`, `sed -i`},
		{`find . -name '*.go' | xargs -I{} sed -i 's/a/b/' {}`, `sed -i`},
		{`find . -name '*.go' -exec sed -i 's/a/b/' {} +`, `sed -i`},
		{`find . -name '*.go' -exec gofmt -w {} \;`, `gofmt -w`},
		{`find . -type f -execdir touch {} \;`, `touch`},
		{`sh -c 'echo x > inner.go'`, `"inner.go"`},
		{`bash -c "sed -i s/a/b/ main.go"`, `sed -i`},
		{`ls | xargs -I{} sh -c 'echo {} >> index.txt'`, `"index.txt"`},
		// Inline scripts that write files.
		{`python3 -c 'import pathlib; pathlib.Path("x.go").write_text("")'`, `inline python3 script calls .write_text`},
		{`python -c "from pathlib import Path; Path('a').write_bytes(b'')"`, `.write_bytes`},
		{`python3 - <<'EOF'
from pathlib import Path
Path("main.go").write_text("package main\n")
EOF`, `inline python3 script calls .write_text`},
		{`python3.12 -c 'open("x","w"); import pathlib; pathlib.Path("y").write_text("")'`, `inline python3.12`},
		{`node -e 'require("fs").writeFileSync("a.js", "")'`, `writefilesync`},
		{`ruby -e 'File.write("a.rb", "")'`, `file.write`},
	} {
		reason := sourceMutation(tc.cmd, guardRoot)
		if reason == "" {
			t.Errorf("allowed %q", tc.cmd)
			continue
		}
		if !strings.Contains(reason, tc.reason) {
			t.Errorf("%q: reason %q, want it to mention %q", tc.cmd, reason, tc.reason)
		}
	}
}

func TestGuardWithoutRootTreatsRelativeWritesAsWorkspace(t *testing.T) {
	if sourceMutation(`echo x > a.go`, "") == "" {
		t.Fatal("without a root, a relative write must count as a workspace write")
	}
	if sourceMutation(`echo x > /dev/null`, "") != "" {
		t.Fatal("/dev/null is never a workspace write")
	}
}

func TestGuardResolvesSymlinkedRoots(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if sourceMutation(`echo x > `+filepath.Join(real, "a.go"), link) == "" {
		t.Fatal("a write through the real path of a symlinked root is still inside it")
	}
	if sourceMutation(`echo x > `+filepath.Join(link, ".orrery", "x"), link) != "" {
		t.Fatal(".orrery under a symlinked root is scratch space")
	}
}

func TestGuardNestedShellDepthIsBounded(t *testing.T) {
	cmd := `echo x > deep.go`
	for range 8 {
		cmd = `sh -c ` + shellQuote(cmd)
	}
	// Beyond the nesting bound the inner script is not inspected; the guard
	// must terminate rather than recurse without limit.
	_ = sourceMutation(cmd, guardRoot)
	shallow := `sh -c ` + shellQuote(`sh -c `+shellQuote(`echo x > deep.go`))
	if sourceMutation(shallow, guardRoot) == "" {
		t.Fatal("two levels of sh -c must still be inspected")
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestExecRejectionNamesTheCause(t *testing.T) {
	root := t.TempDir()
	r := New(root)
	_, err := r.Call(context.Background(), "exec", map[string]any{"command": "echo x > config.go"})
	if err == nil {
		t.Fatal("a workspace write must be rejected")
	}
	for _, want := range []string{`output redirect > to "config.go"`, "edit tool", "/dev/null"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, "config.go")); !os.IsNotExist(statErr) {
		t.Fatal("a rejected command must not run")
	}
}

func TestExecRunsCommandsTheGuardAllows(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(root)
	v, err := r.Call(context.Background(), "exec", map[string]any{"command": `echo ---; cat a.txt 2>/dev/null; echo done > .orrery/note`})
	if err != nil {
		t.Fatal(err)
	}
	if out := v.(map[string]any)["summary"].(string); !strings.Contains(out, "hello") {
		t.Fatalf("summary = %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(root, ".orrery", "note")); err != nil || strings.TrimSpace(string(b)) != "done" {
		t.Fatalf("scratch write: %q %v", b, err)
	}
}
