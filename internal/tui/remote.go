package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ductone/orrey/internal/store"
)

const (
	remoteRequestTimeout = 30 * time.Second
	remoteMaxBody        = 64 << 20
	remoteMaxBatch       = 512
	remoteBackoffMin     = 250 * time.Millisecond
	remoteBackoffMax     = 5 * time.Second
)

// Remote speaks the /api/v1 HTTP and SSE contract of `orrery serve`.
type Remote struct {
	base string
	// display is base with any userinfo password redacted.
	display string
	client  *http.Client
}

// NewRemote targets an orrery server at baseURL. A nil client gets one with
// no overall timeout, since event streams are long-lived; non-stream calls
// are bounded per request instead.
func NewRemote(baseURL string, client *http.Client) (*Remote, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("server url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("server url %q must look like http(s)://host[:port][/prefix]", baseURL)
	}
	if client == nil {
		client = &http.Client{}
	}
	return &Remote{base: strings.TrimRight(u.String(), "/"), display: strings.TrimRight(u.Redacted(), "/"), client: client}, nil
}

// remoteError carries a failed call's status and the server's plain-text
// error body.
type remoteError struct {
	status  int
	message string
}

func (e *remoteError) Error() string { return e.message }

func (r *Remote) Describe() string { return r.display }

func (r *Remote) Session(ctx context.Context, id string) (store.Session, error) {
	var x store.Session
	err := r.call(ctx, http.MethodGet, remoteSessionPath(id, ""), nil, &x)
	var re *remoteError
	if errors.As(err, &re) && re.status == http.StatusNotFound {
		return store.Session{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return x, err
}

func (r *Remote) Lookup(ctx context.Context, integration, externalID, incarnation string) (store.Session, error) {
	if externalID == "" {
		return store.Session{}, errors.New("lookup requires an external id")
	}
	if integration == "" {
		integration = "squire"
	}
	q := url.Values{"integration": {integration}, "external_id": {externalID}, "incarnation": {incarnation}}
	var xs []store.Session
	if err := r.call(ctx, http.MethodGet, "/api/v1/sessions?"+q.Encode(), nil, &xs); err != nil {
		return store.Session{}, err
	}
	// Match client-side too: a server predating the external_id filter
	// answers with every session, and binding to the wrong one is worse
	// than not finding any.
	for _, x := range xs {
		if x.Integration == integration && x.ExternalID == externalID && x.ExternalIncarnation == incarnation {
			return x, nil
		}
	}
	return store.Session{}, fmt.Errorf("%w: %s/%s", ErrNotFound, integration, externalID)
}

type remoteCreate struct {
	Integration         string          `json:"integration"`
	ExternalID          string          `json:"external_id"`
	ExternalIncarnation string          `json:"external_incarnation,omitempty"`
	RequestID           string          `json:"request_id"`
	Prompt              string          `json:"prompt"`
	Workspace           remoteWorkspace `json:"workspace"`
	Budget              *remoteBudget   `json:"budget,omitempty"`
	Routing             *remoteRouting  `json:"routing,omitempty"`
}
type remoteWorkspace struct {
	Path string `json:"path,omitempty"`
	Mode string `json:"mode"`
}
type remoteBudget struct {
	MaxUSD float64 `json:"max_usd"`
}
type remoteRouting struct {
	TierPin string `json:"tier_pin"`
}

func (r *Remote) Create(ctx context.Context, req CreateRequest) (string, error) {
	if req.Integration == "" || req.ExternalID == "" {
		return "", errors.New("remote sessions require an external identity (--session or --external-id)")
	}
	in := remoteCreate{
		Integration: req.Integration, ExternalID: req.ExternalID, ExternalIncarnation: req.ExternalIncarnation,
		RequestID: uuid.NewString(), Prompt: req.Prompt,
		Workspace: remoteWorkspace{Path: req.Workspace, Mode: "shared-write"},
	}
	if req.BudgetUSD > 0 {
		in.Budget = &remoteBudget{MaxUSD: req.BudgetUSD}
	}
	if req.TierPin != "" {
		in.Routing = &remoteRouting{TierPin: req.TierPin}
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := r.call(ctx, http.MethodPost, "/api/v1/sessions", in, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("server accepted the session without returning an id")
	}
	return out.ID, nil
}

func (r *Remote) Send(ctx context.Context, sessionID, content, requestID string) (SendResult, error) {
	if requestID == "" {
		requestID = uuid.NewString()
	}
	in := map[string]string{"request_id": requestID, "content": content}
	var out struct {
		Queued    bool `json:"queued"`
		Duplicate bool `json:"duplicate"`
	}
	if err := r.call(ctx, http.MethodPost, remoteSessionPath(sessionID, "/messages"), in, &out); err != nil {
		return SendResult{}, err
	}
	return SendResult{Queued: out.Queued, Duplicate: out.Duplicate}, nil
}

func (r *Remote) Cancel(ctx context.Context, sessionID string) (bool, error) {
	var out struct {
		Cancelled bool `json:"cancelled"`
	}
	err := r.call(ctx, http.MethodPost, remoteSessionPath(sessionID, "/cancel"), nil, &out)
	return out.Cancelled, err
}

func (r *Remote) Compact(ctx context.Context, sessionID string) error {
	var out struct {
		Compacted bool `json:"compacted"`
	}
	if err := r.call(ctx, http.MethodPost, remoteSessionPath(sessionID, "/compact"), struct{}{}, &out); err != nil {
		return err
	}
	if !out.Compacted {
		return errors.New("server did not compact the session")
	}
	return nil
}

func (r *Remote) Checkpoint(ctx context.Context, sessionID, label string) (store.Checkpoint, error) {
	var x store.Checkpoint
	err := r.call(ctx, http.MethodPost, remoteSessionPath(sessionID, "/checkpoint"), map[string]string{"label": label}, &x)
	return x, err
}

func (r *Remote) Checkpoints(ctx context.Context, sessionID string) ([]store.Checkpoint, error) {
	var xs []store.Checkpoint
	if err := r.call(ctx, http.MethodGet, remoteSessionPath(sessionID, "/checkpoints"), nil, &xs); err != nil {
		return nil, err
	}
	if xs == nil {
		xs = []store.Checkpoint{}
	}
	return xs, nil
}

func (r *Remote) Restore(ctx context.Context, sessionID, checkpointID string) error {
	var out struct {
		Restored bool `json:"restored"`
	}
	if err := r.call(ctx, http.MethodPost, remoteSessionPath(sessionID, "/restore"), map[string]string{"checkpoint_id": checkpointID}, &out); err != nil {
		return err
	}
	if !out.Restored {
		return errors.New("server did not restore the checkpoint")
	}
	return nil
}

func (r *Remote) AddBudget(ctx context.Context, sessionID string, addUSD float64) (bool, error) {
	var out struct {
		Resumed bool `json:"resumed"`
	}
	err := r.call(ctx, http.MethodPost, remoteSessionPath(sessionID, "/budget"), map[string]float64{"add_usd": addUSD}, &out)
	return out.Resumed, err
}

// Stream follows the session's SSE feed, reconnecting from the last
// delivered sequence after any disconnect until ctx ends.
func (r *Remote) Stream(ctx context.Context, sessionID string, after int, out chan<- []Event) error {
	backoff := remoteBackoffMin
	for {
		connected, _ := r.stream(ctx, sessionID, &after, out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			backoff = remoteBackoffMin
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		backoff = min(2*backoff, remoteBackoffMax)
	}
}

// stream runs one SSE connection. Frames already buffered are coalesced into
// a single batch; *after advances only once a batch has been delivered.
func (r *Remote) stream(ctx context.Context, sessionID string, after *int, out chan<- []Event) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+remoteSessionPath(sessionID, "/events")+"?after="+strconv.Itoa(*after), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	res, err := r.client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, remoteMaxBody))
		return false, remoteStatusError(res.StatusCode, raw)
	}
	br := bufio.NewReader(res.Body)
	var (
		batch   []Event
		data    []byte
		hasData bool
		last    = *after
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		select {
		case out <- batch:
		case <-ctx.Done():
			return ctx.Err()
		}
		*after = last
		batch = nil
		return nil
	}
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Oversized line: copy the buffered head before reading on,
			// because the next read reuses the reader's buffer.
			head := append([]byte(nil), line...)
			rest, rerr := br.ReadBytes('\n')
			line, err = append(head, rest...), rerr
		}
		if err != nil {
			if ferr := flush(); ferr != nil {
				return true, ferr
			}
			return true, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if field, value, _ := bytes.Cut(line, []byte(":")); string(field) == "data" {
				if hasData {
					data = append(data, '\n')
				}
				data, hasData = append(data, bytes.TrimPrefix(value, []byte(" "))...), true
			}
			continue
		}
		if hasData {
			var ev Event
			if json.Unmarshal(data, &ev) == nil && ev.Seq > last {
				batch = append(batch, ev)
				last = ev.Seq
			}
			data, hasData = data[:0], false
		}
		if len(batch) < remoteMaxBatch {
			if buffered, _ := br.Peek(br.Buffered()); bytes.Contains(buffered, []byte("\n\n")) || bytes.Contains(buffered, []byte("\r\n\r\n")) {
				continue
			}
		}
		if err := flush(); err != nil {
			return true, err
		}
	}
}

func (r *Remote) call(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, remoteRequestTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, remoteMaxBody))
	if err != nil {
		return err
	}
	// The server reports some failures as a plain-text body with status 200,
	// so a non-JSON body is an error regardless of status.
	if res.StatusCode/100 != 2 || strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || json.Unmarshal(raw, out) != nil {
		return remoteStatusError(res.StatusCode, raw)
	}
	return nil
}

func remoteStatusError(status int, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = fmt.Sprintf("server returned %d %s", status, http.StatusText(status))
	}
	return &remoteError{status: status, message: msg}
}

func remoteSessionPath(id, suffix string) string {
	return "/api/v1/sessions/" + url.PathEscape(id) + suffix
}
