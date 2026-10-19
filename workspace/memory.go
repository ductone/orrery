package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is an FS held in memory, for tests and short-lived workspaces.
type Memory struct {
	mu    sync.Mutex
	files map[string]*memEntry // by io/fs path; "." is the root directory
	// marks are snapshots taken by Mark.
	marks   map[string]map[string]memEntry
	markSeq int
}

type memEntry struct {
	data    []byte
	mode    fs.FileMode
	modTime time.Time
}

// NewMemory returns an empty in-memory filesystem.
func NewMemory() *Memory {
	return &Memory{files: map[string]*memEntry{".": {mode: fs.ModeDir | 0o755}}}
}

func memErr(op, name string, err error) error { return &fs.PathError{Op: op, Path: name, Err: err} }

func (m *Memory) lookup(op, name string) (*memEntry, error) {
	if !fs.ValidPath(name) {
		return nil, memErr(op, name, fs.ErrInvalid)
	}
	e := m.files[name]
	if e == nil {
		return nil, memErr(op, name, fs.ErrNotExist)
	}
	return e, nil
}

func (m *Memory) ReadFile(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.lookup("read", name)
	if err != nil {
		return nil, err
	}
	if e.mode.IsDir() {
		return nil, memErr("read", name, fs.ErrInvalid)
	}
	return append([]byte(nil), e.data...), nil
}

func (m *Memory) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	return memInfo{name: path.Base(name), e: *e}, nil
}

func (m *Memory) ReadDir(_ context.Context, name string) ([]fs.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.lookup("readdir", name)
	if err != nil {
		return nil, err
	}
	if !e.mode.IsDir() {
		return nil, memErr("readdir", name, fs.ErrInvalid)
	}
	var out []fs.DirEntry
	for p, child := range m.files {
		if p != "." && parentOf(p) == name {
			out = append(out, fs.FileInfoToDirEntry(memInfo{name: path.Base(p), e: *child}))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

func (m *Memory) requireDir(op, name string) error {
	parent, err := m.lookup(op, name)
	if err != nil {
		return err
	}
	if !parent.mode.IsDir() {
		return memErr(op, name, fs.ErrInvalid)
	}
	return nil
}

func (m *Memory) WriteFile(_ context.Context, name string, data []byte, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !fs.ValidPath(name) || name == "." {
		return memErr("write", name, fs.ErrInvalid)
	}
	if err := m.requireDir("write", parentOf(name)); err != nil {
		return err
	}
	if e := m.files[name]; e != nil && e.mode.IsDir() {
		return memErr("write", name, fs.ErrInvalid)
	}
	if e := m.files[name]; e != nil {
		perm = e.mode
	}
	m.files[name] = &memEntry{data: append([]byte(nil), data...), mode: perm & fs.ModePerm, modTime: time.Now()}
	return nil
}

func (m *Memory) MkdirAll(_ context.Context, name string, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !fs.ValidPath(name) {
		return memErr("mkdir", name, fs.ErrInvalid)
	}
	if name == "." {
		return nil
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		if e := m.files[p]; e != nil {
			if !e.mode.IsDir() {
				return memErr("mkdir", p, fs.ErrInvalid)
			}
			continue
		}
		m.files[p] = &memEntry{mode: fs.ModeDir | perm&fs.ModePerm, modTime: time.Now()}
	}
	return nil
}

func (m *Memory) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.lookup("remove", name)
	if err != nil {
		return err
	}
	if name == "." {
		return memErr("remove", name, fs.ErrInvalid)
	}
	if e.mode.IsDir() {
		for p := range m.files {
			if parentOf(p) == name && p != "." {
				return memErr("remove", name, fs.ErrExist)
			}
		}
	}
	delete(m.files, name)
	return nil
}

func (m *Memory) RemoveAll(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !fs.ValidPath(name) || name == "." {
		return memErr("removeall", name, fs.ErrInvalid)
	}
	for p := range m.files {
		if p == name || strings.HasPrefix(p, name+"/") {
			delete(m.files, p)
		}
	}
	return nil
}

func (m *Memory) Rename(_ context.Context, oldname, newname string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.lookup("rename", oldname); err != nil {
		return err
	}
	if !fs.ValidPath(newname) || oldname == "." || newname == "." || strings.HasPrefix(newname, oldname+"/") {
		return memErr("rename", newname, fs.ErrInvalid)
	}
	if err := m.requireDir("rename", parentOf(newname)); err != nil {
		return err
	}
	moved := map[string]*memEntry{}
	for p, e := range m.files {
		if p == oldname || strings.HasPrefix(p, oldname+"/") {
			moved[newname+p[len(oldname):]] = e
			delete(m.files, p)
		}
	}
	for p, e := range moved {
		m.files[p] = e
	}
	return nil
}

type memInfo struct {
	name string
	e    memEntry
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return int64(len(i.e.data)) }
func (i memInfo) Mode() fs.FileMode  { return i.e.mode }
func (i memInfo) ModTime() time.Time { return i.e.modTime }
func (i memInfo) IsDir() bool        { return i.e.mode.IsDir() }
func (i memInfo) Sys() any           { return nil }

// snapshot copies the tree. Callers hold m.mu.
func (m *Memory) snapshot() map[string]memEntry {
	out := make(map[string]memEntry, len(m.files))
	for p, e := range m.files {
		out[p] = memEntry{data: append([]byte(nil), e.data...), mode: e.mode, modTime: e.modTime}
	}
	return out
}

// changedPaths lists the paths whose entries differ between two trees.
func changedPaths(a, b map[string]memEntry) []string {
	var out []string
	for p, x := range a {
		y, ok := b[p]
		if !ok || x.mode != y.mode || string(x.data) != string(y.data) {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Isolate gives a worker a copy of the tree. On a conflict the copy is kept
// in memory and reported as "memory:<jobID>".
func (m *Memory) Isolate(_ context.Context, jobID string) (Workspace, FinishFunc, error) {
	m.mu.Lock()
	base := m.snapshot()
	child := &Memory{files: map[string]*memEntry{}}
	for p, e := range base {
		e := e
		e.data = append([]byte(nil), e.data...)
		child.files[p] = &e
	}
	m.mu.Unlock()

	finish := func(_ context.Context, merge bool) (MergeResult, error) {
		child.mu.Lock()
		worker := child.snapshot()
		child.mu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		res := MergeResult{Changed: changedPaths(base, worker)}
		if !merge {
			return res, nil
		}
		res.Conflicting = Conflicts(res.Changed, changedPaths(base, m.snapshot()))
		if len(res.Conflicting) > 0 {
			res.Kept = "memory:" + jobID
			return res, nil
		}
		for _, p := range res.Changed {
			if e, ok := worker[p]; ok {
				e := e
				m.files[p] = &e
			} else {
				delete(m.files, p)
			}
		}
		res.Merged = true
		return res, nil
	}
	return Workspace{FS: child, Isolator: child, Changes: child}, finish, nil
}

// Mark records the tree's current state.
func (m *Memory) Mark(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.marks == nil {
		m.marks = map[string]map[string]memEntry{}
	}
	m.markSeq++
	id := strconv.Itoa(m.markSeq)
	m.marks[id] = m.snapshot()
	return id, nil
}

// ChangedSince reports the files that differ from a mark.
func (m *Memory) ChangedSince(_ context.Context, mark string) ([]FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	base, ok := m.marks[mark]
	if !ok {
		return nil, fmt.Errorf("workspace: unknown mark %q", mark)
	}
	now := m.snapshot()
	var out []FileChange
	for _, p := range changedPaths(base, now) {
		before, hadBefore := base[p]
		after, hasAfter := now[p]
		if (hadBefore && before.mode.IsDir()) || (hasAfter && after.mode.IsDir()) {
			continue
		}
		c := FileChange{Path: p}
		if hadBefore {
			c.Before = before.data
			if c.Before == nil {
				c.Before = []byte{}
			}
		}
		if hasAfter {
			c.After = after.data
			if c.After == nil {
				c.After = []byte{}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// Release discards a mark.
func (m *Memory) Release(_ context.Context, mark string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.marks, mark)
	return nil
}

var (
	_ Isolator      = (*Memory)(nil)
	_ ChangeTracker = (*Memory)(nil)
)
