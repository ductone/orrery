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

// workspaceWrites are commands that write files inside the workspace. The
// guard used to reject them; the final diff is reviewed independently
// whatever wrote it, so they are allowed.
var workspaceWrites = []string{
	`gofmt -w .`,
	`gofmt -w main.go`,
	`goimports -w .`,
	`go fmt ./...`,
	`echo x > config.go`,
	`echo x >> CHANGELOG.md`,
	`printf 'a\n' > notes.txt`,
	`sed -i 's/a/b/' main.go`,
	`sed -i.bak 's/a/b/' main.go`,
	`perl -pi -e 's/a/b/' main.go`,
	`echo x | tee out.txt`,
	`touch new.go`,
	`python3 -c 'import pathlib; pathlib.Path("x.go").write_text("")'`,
	`find . -name '*.go' -exec gofmt -w {} +`,
	`sh -c 'echo x > inner.go'`,
}

func TestGuardAllowsWorkspaceWrites(t *testing.T) {
	for _, cmd := range workspaceWrites {
		if reason := destructiveCommand(cmd, guardRoot); reason != "" {
			t.Errorf("rejected %q: %s", cmd, reason)
		}
	}
	// Without a root, a relative write is still just a write.
	if reason := destructiveCommand(`echo x > a.go`, ""); reason != "" {
		t.Fatalf("a relative write must be allowed: %s", reason)
	}
	if reason := destructiveCommand(`echo x > /dev/null`, ""); reason != "" {
		t.Fatalf("/dev/null must be allowed: %s", reason)
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if reason := destructiveCommand(`echo x > `+filepath.Join(real, "a.go"), link); reason != "" {
		t.Fatalf("a write through a symlinked root must be allowed: %s", reason)
	}
	if reason := destructiveCommand(`echo x > `+filepath.Join(link, ".orrery", "x"), link); reason != "" {
		t.Fatalf("a scratch write must be allowed: %s", reason)
	}
}

func TestGuardAllowsOrdinaryGit(t *testing.T) {
	for _, cmd := range []string{
		`git status`,
		`git status --porcelain`,
		`git diff`,
		`git diff --stat HEAD~3`,
		`git add .`,
		`git add -A internal/tools`,
		`git commit -m "msg"`,
		`git log --oneline -20`,
		`git show HEAD`,
		`git checkout -b feature`,
		`git checkout -B feature`,
		`git checkout main`,
		`git checkout --theirs main.go`,
		`git switch -c feature`,
		`git reset HEAD`,
		`git reset --soft HEAD~1`,
		`git reset --mixed HEAD`,
		`git clean -n`,
		`git clean --dry-run -d`,
		`git restore --staged main.go`,
		`git stash`,
		`git stash push -m wip`,
		`git stash pop`,
		`git push`,
		`git push origin main`,
		`git push --force-with-lease`,
		`git push --force-with-lease=main`,
		`git -C /work/other status`,
		`rm file.go`,
		`rm -r internal/tools`,
		`rm -rf ./build`,
		`rm -rf /tmp/scratch`,
		`rm -rf build/*`,
		`rm -rf sub/*.o`,
		`rm *.tmp`,
		`rm -rf /tmp/*`,
		`rm -rf /tmp`,
		`rm -rf ` + filepath.Join(os.TempDir(), "*"),
		`rm -rf ` + os.TempDir(),
	} {
		if reason := destructiveCommand(cmd, guardRoot); reason != "" {
			t.Errorf("rejected %q: %s", cmd, reason)
		}
	}
}

func TestGuardRejectsDestructiveCommands(t *testing.T) {
	for _, tc := range []struct{ cmd, reason string }{
		// git forms that discard work.
		{`git reset --hard`, `git reset --hard`},
		{`git reset --hard HEAD~1`, `git reset --hard`},
		{`git -C /work/repo reset --hard`, `git reset --hard`},
		{`git clean -f`, `git clean -f`},
		{`git clean --force`, `git clean -f`},
		{`git clean -fd`, `git clean -f`},
		{`git clean -fdx`, `git clean -f`},
		{`git clean -xffd`, `git clean -f`},
		{`git checkout -- main.go`, `git checkout --`},
		{`git checkout -- .`, `git checkout --`},
		{`git checkout -- internal/a.go internal/b.go`, `git checkout --`},
		{`git checkout .`, `git checkout --`},
		{`git checkout ./`, `git checkout --`},
		{`git restore main.go`, `git restore`},
		{`git restore .`, `git restore`},
		{`git restore --worktree main.go`, `git restore`},
		{`git restore --source=HEAD main.go`, `git restore`},
		{`git restore --staged --worktree main.go`, `git restore`},
		{`git stash drop`, `git stash drop`},
		{`git stash drop stash@{1}`, `git stash drop`},
		{`git stash clear`, `git stash clear`},
		{`git push --force`, `git push --force`},
		{`git push -f`, `git push --force`},
		{`git push --force origin main`, `git push --force`},
		{`git push -uf origin main`, `git push --force`},
		{`git push origin +main`, `git push --force`},
		// recursive removal of the workspace, its parents, or paths outside it.
		{`rm -rf /work/repo`, `rm -r`},
		{`rm -rf /work/repo/`, `rm -r`},
		{`rm -rf .`, `rm -r`},
		{`rm -rf ./`, `rm -r`},
		{`rm -r /work`, `rm -r`},
		{`rm -rf /`, `rm -r`},
		{`rm -rf ~`, `rm -r`},
		{`rm -rf ~/code`, `rm -r`},
		{`rm -rf /home/dev/other`, `rm -r`},
		{`rm -rf ../sibling`, `rm -r`},
		{`rm -rf ..`, `rm -r`},
		{`rm --recursive /work/repo`, `rm -r`},
		{`rm -rf *`, `rm -r`},
		{`rm -rf ./*`, `rm -r`},
		{`rm -r * .*`, `rm -r`},
		{`rm -rf .[!.]* *`, `rm -r`},
		{`rm -rf ../*`, `rm -r`},
		{`cd sub && rm -rf ../*`, `rm -r`},
		{`rm -rf ?`, `rm -r`},
		{`rm -rf [ab]*`, `rm -r`},
		{`rm -rf $ROOT/*`, `rm -r`},
		{`rm -rf "$DIR"`, `rm -r`},
		// indirection: shells, wrappers, cd, and find -exec.
		{`sh -c 'git reset --hard'`, `git reset --hard`},
		{`bash -c "git clean -fd"`, `git clean -f`},
		{`env LC_ALL=C git checkout -- main.go`, `git checkout --`},
		{`timeout 10 git push --force`, `git push --force`},
		{`cd /work/repo && git reset --hard`, `git reset --hard`},
		{`cd internal && git checkout -- moved.go`, `git checkout --`},
		{`cd /tmp && rm -rf /work/repo`, `rm -r`},
		{`cd / && rm -rf work/repo`, `rm -r`},
		{`find . -exec rm -rf {} +`, `rm -r`},
		{`find /work/repo -name '*.go' -exec rm -rf {} \;`, `rm -r`},
		{`sudo git reset --hard`, `git reset --hard`},
		{`/usr/bin/git clean -f`, `git clean -f`},
	} {
		reason := destructiveCommand(tc.cmd, guardRoot)
		if reason == "" {
			t.Errorf("allowed %q", tc.cmd)
			continue
		}
		if !strings.Contains(reason, tc.reason) {
			t.Errorf("%q: reason %q, want it to mention %q", tc.cmd, reason, tc.reason)
		}
	}
}

func TestGuardNestedShellDepthIsBounded(t *testing.T) {
	cmd := `git reset --hard`
	for range 8 {
		cmd = `sh -c "` + strings.ReplaceAll(cmd, `"`, `\"`) + `"`
	}
	// Beyond the nesting bound the inner script is not inspected; the guard
	// must terminate rather than recurse without limit.
	_ = destructiveCommand(cmd, guardRoot)
	shallow := `sh -c "sh -c 'git reset --hard'"`
	if destructiveCommand(shallow, guardRoot) == "" {
		t.Fatal("two levels of sh -c must still be inspected")
	}
}

func TestExecRejectionNamesTheCause(t *testing.T) {
	root := t.TempDir()
	r := New(root)
	_, err := r.Call(context.Background(), "exec", map[string]any{"command": "git reset --hard"})
	if err == nil {
		t.Fatal("a destructive command must be rejected")
	}
	for _, want := range []string{"git reset --hard", "would discard work that cannot be recovered", "leave them alone, or ask"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
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
	// A formatter-style workspace write runs instead of being rejected.
	if _, err := r.Call(context.Background(), "exec", map[string]any{"command": `echo formatted > a.txt`}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || strings.TrimSpace(string(b)) != "formatted" {
		t.Fatalf("workspace write: %q %v", b, err)
	}
}
