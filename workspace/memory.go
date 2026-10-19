package workspace

import (
	"context"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an FS held in memory, for tests and short-lived workspaces.
type Memory struct {
	mu    sync.Mutex
	files map[string]*memEntry // by io/fs path; "." is the root directory
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
