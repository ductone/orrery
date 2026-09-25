package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Squire runs the harness-side half of Squire's agent contract, matching
// what Squire's omp extensions provide so an Orrery harness adapter needs no
// screen scraping:
//
//   - pids/<pid>.json binds this process to its Orrery session
//     ({pid, started_at, updated_at, current_session}), written atomically
//     and removed on exit;
//   - ipc/<task>.sock accepts newline-delimited {"id","type":"prompt",
//     "message"} commands and answers {"id","type":"response","command",
//     "success","error"}; only "prompt" is served;
//   - sessions/<session>.jsonl mirrors the session's durable event log, one
//     Event per line, for usage and transcript ingestion.
type Squire struct {
	dir, taskID string
	pidPath     string
	started     time.Time
	listener    net.Listener
	socketPath  string

	mu      sync.Mutex
	journal *os.File
	wg      sync.WaitGroup
}

const maxControlLine = 2 << 20

// StartSquire binds the control socket. submit delivers one prompt and
// returns once the engine accepted it (or failed to); it must be safe to call
// from multiple goroutines.
func StartSquire(dir, taskID string, submit func(string) error) (*Squire, error) {
	if dir == "" || taskID == "" {
		return nil, errors.New("squire integration requires a directory and task id")
	}
	s := &Squire{dir: dir, taskID: taskID, started: time.Now().UTC()}
	s.pidPath = filepath.Join(dir, "pids", strconv.Itoa(os.Getpid())+".json")
	ipc := filepath.Join(dir, "ipc")
	// Every file here is written by path, so each directory must be private
	// to this user before anything is created in it.
	for _, d := range []string{dir, ipc, filepath.Dir(s.pidPath), filepath.Join(dir, "sessions")} {
		if err := privateDir(d); err != nil {
			return nil, err
		}
	}
	s.socketPath = filepath.Join(ipc, taskID+".sock")
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return nil, fmt.Errorf("bind squire control socket: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	s.listener = l
	s.wg.Add(1)
	go s.accept(submit)
	return s, nil
}

func (s *Squire) SocketPath() string { return s.socketPath }

func (s *Squire) accept(submit func(string) error) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go serveControl(conn, submit)
	}
}

type controlCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

type controlResponse struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func serveControl(conn net.Conn, submit func(string) error) {
	defer conn.Close()
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 64<<10), maxControlLine)
	enc := json.NewEncoder(conn)
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			continue
		}
		_ = enc.Encode(dispatchControl(line, submit))
	}
}

func dispatchControl(line []byte, submit func(string) error) controlResponse {
	var cmd controlCommand
	if err := json.Unmarshal(line, &cmd); err != nil {
		return controlResponse{Type: "response", Command: "parse", Error: "malformed JSON"}
	}
	if cmd.Type == "" {
		return controlResponse{Type: "response", Command: "parse", Error: "missing command type"}
	}
	res := controlResponse{ID: cmd.ID, Type: "response", Command: cmd.Type}
	switch {
	case cmd.Type != "prompt":
		res.Error = "unknown command " + cmd.Type
	case cmd.Message == "":
		res.Error = "message is required"
	default:
		if err := submit(cmd.Message); err != nil {
			res.Error = err.Error()
		} else {
			res.Success = true
		}
	}
	return res
}

// Bind publishes the pid→session binding and starts a fresh journal for the
// session. The TUI replays the full event log after binding, so truncating
// keeps the journal an exact mirror.
func (s *Squire) Bind(sessionID string) error {
	body, err := json.Marshal(map[string]any{
		"pid":             os.Getpid(),
		"started_at":      s.started.Format(time.RFC3339Nano),
		"updated_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"current_session": sessionID,
	})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.pidPath), ".binding-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(body)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), s.pidPath)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return werr
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "sessions", filepath.Base(sessionID)+".jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.journal != nil {
		_ = s.journal.Close()
	}
	s.journal = f
	s.mu.Unlock()
	return nil
}

// Record appends events to the journal. Journal failures never interrupt the
// session; the SQLite store remains the system of record.
func (s *Squire) Record(events []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return
	}
	w := bufio.NewWriter(s.journal)
	enc := json.NewEncoder(w)
	for _, ev := range events {
		_ = enc.Encode(ev)
	}
	_ = w.Flush()
}

func (s *Squire) Close() error {
	err := s.listener.Close()
	s.wg.Wait()
	_ = os.Remove(s.socketPath)
	_ = os.Remove(s.pidPath)
	s.mu.Lock()
	if s.journal != nil {
		_ = s.journal.Close()
		s.journal = nil
	}
	s.mu.Unlock()
	return err
}

// privateDir creates dir with mode 0700, or tightens an existing one, and
// refuses symlinks and directories owned by another user. Ownership is
// checked before chmod so a planted symlink can never redirect it.
func privateDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%s is owned by another user", dir)
	}
	return os.Chmod(dir, 0o700)
}
