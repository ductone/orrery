// Package outcomes attributes session events to model routes: who authored
// work a review passed or failed, which reviewers were overturned, which
// answers were rejected, and how runs ended. The engine applies it to live
// events, and a store can replay stored events through it to backfill.
package outcomes

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/ductone/orrery/storage"
)

// session is the per-session state needed to attribute outcomes.
type session struct {
	route         string             // active route from the latest routing decision
	answerRoute   string             // route that wrote the latest assistant message
	weights       map[string]float64 // edit and exec-write calls since the last review
	reviewers     []string           // review job routes since the last review outcome
	lastShares    map[string]float64 // authors of the last reviewed diff
	lastReviewers []string
	lastFailed    bool // last review failed
}

// Projector folds events into per-route outcome deltas. It keeps per-session
// state in memory and is safe for concurrent use.
type Projector struct {
	mu       sync.Mutex
	sessions map[string]*session
}

// New returns an empty Projector.
func New() *Projector {
	return &Projector{sessions: map[string]*session{}}
}

var execWrite = regexp.MustCompile(`(^|[\s;&|(])(sed\s+-[a-z]*i|tee|mv|cp|rm|touch|patch|git\s+(apply|checkout|restore|mv|rm)|gofmt\s+-w|goimports\s+-w|go\s+mod\s+tidy)(\s|$)|[^0-9&>=-]>>?\s*[^&\s]`)

var execNoise = strings.NewReplacer("2>&1", "", ">/dev/null", "", "> /dev/null", "", "2>/dev/null", "")

func execWrites(command string) bool {
	return execWrite.MatchString(" " + execNoise.Replace(command))
}

// deltas accumulates outcome changes per route in first-touched order.
type deltas struct {
	order []string
	by    map[string]*storage.ModelOutcome
}

func (d *deltas) route(r string) *storage.ModelOutcome {
	if d.by == nil {
		d.by = map[string]*storage.ModelOutcome{}
	}
	o := d.by[r]
	if o == nil {
		o = &storage.ModelOutcome{Route: r}
		d.by[r] = o
		d.order = append(d.order, r)
	}
	return o
}

func (d *deltas) list() []storage.ModelOutcome {
	out := make([]storage.ModelOutcome, 0, len(d.order))
	for _, r := range d.order {
		out = append(out, *d.by[r])
	}
	return out
}

// Apply records an event of type typ with JSON data raw for session sid and
// returns the outcome changes it causes, one entry per affected route. Each
// entry's fields are amounts to add (authored shares can be negative when a
// review is overturned). Most events cause none.
func (p *Projector) Apply(sid, typ string, raw []byte) []storage.ModelOutcome {
	switch typ {
	case "routing.decision", "routing.selected", "job.started", "tool.finished", "assistant.message", "review.outcome", "completion.answer_check", "session.terminal":
	default:
		return nil
	}
	var d struct {
		Model    string `json:"model"`
		Review   bool   `json:"review"`
		Pass     bool   `json:"pass"`
		Inconcl  bool   `json:"inconclusive"`
		Disputed bool   `json:"disputed"`
		OffTopic bool   `json:"off_topic"`
		Status   string `json:"status"`
		Decision struct {
			Model struct {
				ID string `json:"id"`
			} `json:"model"`
		} `json:"decision"`
		Call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"call"`
		Result       any  `json:"result"`
		Deduplicated bool `json:"deduplicated"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[sid]
	if s == nil {
		s = &session{weights: map[string]float64{}}
		p.sessions[sid] = s
	}
	var out deltas
	switch typ {
	case "routing.decision", "routing.selected":
		if d.Decision.Model.ID != "" {
			s.route = d.Decision.Model.ID
		}
	case "assistant.message":
		s.answerRoute = d.Model
		if s.answerRoute == "" {
			s.answerRoute = s.route
		}
	case "job.started":
		if d.Review && d.Model != "" {
			s.reviewers = append(s.reviewers, d.Model)
		}
	case "tool.finished":
		if s.route == "" || d.Deduplicated {
			return nil
		}
		if m, ok := d.Result.(map[string]any); ok && m["ok"] == false {
			return nil
		}
		switch d.Call.Name {
		case "edit":
			s.weights[s.route]++
		case "exec":
			if c, _ := d.Call.Arguments["command"].(string); execWrites(c) {
				s.weights[s.route]++
			}
		}
	case "completion.answer_check":
		if d.OffTopic && s.answerRoute != "" {
			out.route(s.answerRoute).AnswerRejected++
		}
	case "session.terminal":
		if s.answerRoute != "" {
			o := out.route(s.answerRoute)
			switch d.Status {
			case "pass":
				o.RunPass++
			case "fail":
				o.RunFail++
			case "input_required":
				o.RunInputRequired++
			case "cancelled":
				o.RunCancelled++
			default:
				return nil
			}
		}
	case "review.outcome":
		s.review(&out, d.Pass, d.Inconcl, d.Disputed)
	}
	return out.list()
}

func (s *session) review(out *deltas, pass, inconclusive, disputed bool) {
	if inconclusive {
		return
	}
	overturn := func() {
		for _, r := range s.lastReviewers {
			out.route(r).Overturned++
		}
		for r, share := range s.lastShares {
			o := out.route(r)
			o.AuthoredPass += share
			o.AuthoredFail -= share
		}
		s.lastFailed = false
	}
	if disputed {
		// Adjudication re-judges the diff the last review failed. Passing it
		// converts that failure and overturns the original reviewers.
		s.reviewers = nil
		if pass && s.lastFailed {
			overturn()
		}
		return
	}
	for _, r := range s.reviewers {
		out.route(r).Reviews++
	}
	total := 0.0
	for _, w := range s.weights {
		total += w
	}
	if total == 0 {
		// No edits since the last review: the same diff was judged again, so
		// a pass overturns the failed review before it.
		if pass && s.lastFailed {
			overturn()
		} else if !pass {
			s.lastFailed, s.lastReviewers = true, s.reviewers
		}
		s.reviewers = nil
		return
	}
	shares := map[string]float64{}
	for r, w := range s.weights {
		shares[r] = w / total
		o := out.route(r)
		if pass {
			o.AuthoredPass += shares[r]
		} else {
			o.AuthoredFail += shares[r]
		}
	}
	s.lastShares, s.lastReviewers, s.lastFailed = shares, s.reviewers, !pass
	s.weights, s.reviewers = map[string]float64{}, nil
}
