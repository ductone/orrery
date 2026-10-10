// Package classify is the boundary between Orrery's decisions and the
// classifier that answers them. A decision asks typed questions about a state
// (noul: a calibrated probability; choice; score) and reads typed answers, so
// any classifier that can answer them can be plugged in. TypeSafe's Jev
// (internal/jev) is the first implementation.
package classify

import (
	"context"
	"errors"
	"sync"
)

// Question is one named question. Criteria is {"true":..,"false":..} for a
// noul, an option→description map for a choice, and an ordered []string of
// level descriptions for a score.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func Noul(instructions, whenTrue, whenFalse string) Question {
	return Question{Type: "noul", Instructions: instructions, Criteria: map[string]string{"true": whenTrue, "false": whenFalse}}
}

func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

func Score(instructions string, levels ...string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Answer is one typed answer. Noul answers carry only Noul; choice and score
// answers carry Probabilities and Confidence.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Classifier answers questions about a state. Implementations must be safe for
// concurrent use; a failed call returns an error rather than partial answers.
type Classifier interface {
	Ask(ctx context.Context, state any, questions map[string]Question) (Response, error)
}

// Fake is a scripted Classifier for tests: Answer decides each named
// question's answer from the state. It records every call.
type Fake struct {
	Answer func(state any, name string, q Question) (Answer, error)

	mu    sync.Mutex
	Calls []FakeCall
}

// FakeCall is one recorded Ask.
type FakeCall struct {
	State     any
	Questions map[string]Question
}

func (f *Fake) Ask(_ context.Context, state any, questions map[string]Question) (Response, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{State: state, Questions: questions})
	f.mu.Unlock()
	if f.Answer == nil {
		return Response{}, errors.New("classify: fake has no answers")
	}
	out := Response{Model: "fake", Answers: map[string]Answer{}}
	for name, q := range questions {
		a, err := f.Answer(state, name, q)
		if err != nil {
			return Response{}, err
		}
		out.Answers[name] = a
	}
	return out, nil
}

// Prob is a noul answer with probability p.
func Prob(p float64) Answer { return Answer{Type: "noul", Noul: &p} }
