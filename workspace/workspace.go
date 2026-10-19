// Package workspace describes the files an agent works on.
//
// A Workspace has an FS that the engine's file tools (read, write, edit,
// replace, search) and instruction discovery go through, and optionally a
// directory on disk. Features that run programs need the directory: the exec,
// job and ast_edit tools, language servers, verification and review commands,
// git-based change tracking, and isolated worker worktrees. A workspace
// without one (a virtual workspace, such as one stored in a database) gets
// the file tools only.
//
// FS's methods match those of github.com/russellhaering/moraine/vfs.FS, so a
// moraine filesystem can be used directly.
package workspace

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FS is a hierarchical filesystem addressed with io/fs-style paths:
// unrooted, slash-separated, "." for the root. Errors should match the
// io/fs sentinel errors (fs.ErrNotExist and so on) via errors.Is.
type FS interface {
	ReadFile(ctx context.Context, name string) ([]byte, error)
	Stat(ctx context.Context, name string) (fs.FileInfo, error)
	ReadDir(ctx context.Context, name string) ([]fs.DirEntry, error)
	// WriteFile creates or replaces a file. Its parent directory must exist.
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	MkdirAll(ctx context.Context, name string, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
	RemoveAll(ctx context.Context, name string) error
	Rename(ctx context.Context, oldname, newname string) error
}

// Workspace is the files an agent works on.
type Workspace struct {
	// FS holds the workspace's files. A nil FS is a workspace with no files:
	// the engine registers no file tools.
	FS FS
	// Dir is the workspace's directory on disk, or "" for a virtual
	// workspace. When set, FS must present the same files.
	Dir string
	// Isolator, when set on a virtual workspace, gives isolated workers
	// their own copy of the files. Without it they share the workspace.
	// Workspaces on disk isolate workers with git worktrees instead.
	Isolator Isolator
	// Changes, when set on a virtual workspace, reports what a run changed,
	// so review can see it. Workspaces on disk use git instead.
	Changes ChangeTracker
}

// Disk returns the workspace rooted at dir on the local disk.
func Disk(dir string) Workspace {
	return Workspace{FS: OS(dir), Dir: dir}
}

// Virtual returns a workspace whose files live only in fsys. If fsys also
// implements Isolator or ChangeTracker, the workspace uses them.
func Virtual(fsys FS) Workspace {
	ws := Workspace{FS: fsys}
	ws.Isolator, _ = fsys.(Isolator)
	ws.Changes, _ = fsys.(ChangeTracker)
	return ws
}

// Isolator gives a worker its own copy of a workspace.
type Isolator interface {
	// Isolate copies the workspace for the worker job jobID. The worker
	// works in the returned workspace; finish ends the copy.
	Isolate(ctx context.Context, jobID string) (child Workspace, finish FinishFunc, err error)
}

// FinishFunc ends an isolated copy. With merge set, it applies the worker's
// changes to the parent workspace, all or nothing: if any path the worker
// changed was also changed in the parent since the copy was made (the same
// path, or one inside the other), nothing is merged and the copy is kept so
// the work is not lost. Without merge, the copy is discarded.
type FinishFunc func(ctx context.Context, merge bool) (MergeResult, error)

// MergeResult reports how an isolated copy ended.
type MergeResult struct {
	// Changed lists the paths the worker added, modified or removed.
	Changed []string
	Merged  bool
	// Conflicting lists the changed paths the parent also changed.
	Conflicting []string
	// Kept names where an unmerged copy remains, for a person to resolve.
	Kept string
}

// ChangeTracker reports what changed in a workspace since a mark.
type ChangeTracker interface {
	// Mark records the workspace's current state and returns a handle to it.
	Mark(ctx context.Context) (string, error)
	// ChangedSince returns the files that differ from the marked state,
	// sorted by path.
	ChangedSince(ctx context.Context, mark string) ([]FileChange, error)
	// Release discards a mark.
	Release(ctx context.Context, mark string) error
}

// FileChange is one file that differs from a marked state. Before is nil
// for an added file and After is nil for a removed one.
type FileChange struct {
	Path          string
	Before, After []byte
}

// Conflicts returns the paths in changed that overlap a path in other: the
// same path, or one inside the other. Isolator implementations use it to
// decide whether a merge is safe.
func Conflicts(changed, other []string) []string {
	var out []string
	for _, p := range changed {
		for _, q := range other {
			if p == q || strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/") {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// OS returns an FS for the directory tree rooted at dir. Names are resolved
// lexically inside dir; a name that is not a valid io/fs path is rejected.
func OS(dir string) FS {
	return osFS{root: filepath.Clean(dir)}
}

type osFS struct{ root string }

func (o osFS) path(op, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return filepath.Join(o.root, filepath.FromSlash(name)), nil
}

func (o osFS) ReadFile(_ context.Context, name string) ([]byte, error) {
	p, err := o.path("read", name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (o osFS) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	p, err := o.path("stat", name)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (o osFS) ReadDir(_ context.Context, name string) ([]fs.DirEntry, error) {
	p, err := o.path("readdir", name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

func (o osFS) WriteFile(_ context.Context, name string, data []byte, perm fs.FileMode) error {
	p, err := o.path("write", name)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, perm)
}

func (o osFS) MkdirAll(_ context.Context, name string, perm fs.FileMode) error {
	p, err := o.path("mkdir", name)
	if err != nil {
		return err
	}
	return os.MkdirAll(p, perm)
}

func (o osFS) Remove(_ context.Context, name string) error {
	p, err := o.path("remove", name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (o osFS) RemoveAll(_ context.Context, name string) error {
	p, err := o.path("removeall", name)
	if err != nil {
		return err
	}
	if name == "." {
		return errors.New("workspace: refusing to remove the workspace root")
	}
	return os.RemoveAll(p)
}

func (o osFS) Rename(_ context.Context, oldname, newname string) error {
	op, err := o.path("rename", oldname)
	if err != nil {
		return err
	}
	np, err := o.path("rename", newname)
	if err != nil {
		return err
	}
	return os.Rename(op, np)
}

// IOFS returns an io/fs view of fsys whose operations use ctx, so fs.WalkDir
// and similar helpers work on it. Open reads a file's contents into memory.
func IOFS(ctx context.Context, fsys FS) fs.FS {
	return ioFS{fsys: fsys, ctx: ctx}
}

type ioFS struct {
	fsys FS
	ctx  context.Context
}

var (
	_ fs.ReadDirFS  = ioFS{}
	_ fs.ReadFileFS = ioFS{}
	_ fs.StatFS     = ioFS{}
)

func (i ioFS) ReadFile(name string) ([]byte, error)       { return i.fsys.ReadFile(i.ctx, name) }
func (i ioFS) ReadDir(name string) ([]fs.DirEntry, error) { return i.fsys.ReadDir(i.ctx, name) }
func (i ioFS) Stat(name string) (fs.FileInfo, error)      { return i.fsys.Stat(i.ctx, name) }

func (i ioFS) Open(name string) (fs.File, error) {
	info, err := i.fsys.Stat(i.ctx, name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		entries, err := i.fsys.ReadDir(i.ctx, name)
		if err != nil {
			return nil, err
		}
		return &openDir{info: info, entries: entries}, nil
	}
	data, err := i.fsys.ReadFile(i.ctx, name)
	if err != nil {
		return nil, err
	}
	return &openFile{info: info, r: strings.NewReader(string(data))}, nil
}

type openFile struct {
	info fs.FileInfo
	r    *strings.Reader
}

func (f *openFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *openFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *openFile) Close() error               { return nil }

type openDir struct {
	info    fs.FileInfo
	entries []fs.DirEntry
}

func (d *openDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.Name(), Err: errors.New("is a directory")}
}
func (d *openDir) Close() error { return nil }
func (d *openDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}
