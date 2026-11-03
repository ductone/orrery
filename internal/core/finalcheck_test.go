package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFinalCheckRefusesOnceThenFinishes(t *testing.T) {
	var bodies []string
	reply := func(n int, body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		bodies = append(bodies, string(b))
		return responsesText(fmt.Sprintf("answer %d", n))
	}
	var checked []string
	e, _, workspace := handlerEngine(t, reply, WithFinalCheck(func(_ context.Context, sid, result string) string {
		checked = append(checked, result)
		if result == "answer 1" {
			return "Nobody saw that answer: send it."
		}
		return ""
	}))
	res, err := e.Run(context.Background(), handlerRequest(workspace), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Result["answer"]; got != "answer 2" {
		t.Fatalf("answer = %v, want the one after the refusal", got)
	}
	if len(checked) != 2 {
		t.Fatalf("checked %q, want both final results", checked)
	}
	if len(bodies) < 2 || !strings.Contains(bodies[1], "Nobody saw that answer") {
		t.Fatal("the refusal's note did not reach the model")
	}
}

func TestFinalCheckRefusesAtMostTwice(t *testing.T) {
	reply := func(n int, _ map[string]any) map[string]any { return responsesText(fmt.Sprintf("answer %d", n)) }
	calls := 0
	e, _, workspace := handlerEngine(t, reply, WithFinalCheck(func(context.Context, string, string) string {
		calls++
		return "no"
	}))
	res, err := e.Run(context.Background(), handlerRequest(workspace), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Result["answer"]; got != "answer 3" || calls != maxFinalRejections {
		t.Fatalf("answer %v after %d checks, want answer 3 after %d", got, calls, maxFinalRejections)
	}
}
