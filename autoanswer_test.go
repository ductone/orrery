package orrery

import (
	"context"
	"testing"

	"github.com/ductone/orrery/agentproto"
)

func TestAutoAnswer(t *testing.T) {
	budget := func(budgetUSD float64) agentproto.InputRequest {
		return agentproto.InputRequest{Kind: agentproto.InputBudget, BudgetUSD: budgetUSD, ExtensionUSD: 5, Choices: []string{"Add $5.00 and continue", agentproto.ChoiceStop}}
	}
	for _, tc := range []struct {
		name   string
		h      InputHandler
		in     agentproto.InputRequest
		answer string
		ok     bool
	}{
		{"budget within cap extends", AutoAnswer(20, ""), budget(10), "Add $5.00 and continue", true},
		{"budget exactly at cap extends", AutoAnswer(15, ""), budget(10), "Add $5.00 and continue", true},
		{"budget over cap stops", AutoAnswer(14, ""), budget(10), agentproto.ChoiceStop, true},
		{"ask answered", AutoAnswer(20, "use your judgment"), agentproto.InputRequest{Kind: agentproto.InputAsk}, "use your judgment", true},
		{"ask without reply pauses", AutoAnswer(20, ""), agentproto.InputRequest{Kind: agentproto.InputAsk}, "", false},
		{"limit continues", AutoAnswer(20, ""), agentproto.InputRequest{Kind: agentproto.InputLimit}, agentproto.ChoiceContinue, true},
		{"unknown kind pauses", AutoAnswer(20, "x"), agentproto.InputRequest{}, "", false},
	} {
		answer, ok, err := tc.h(context.Background(), "s", tc.in)
		if err != nil || answer != tc.answer || ok != tc.ok {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v)", tc.name, answer, ok, err, tc.answer, tc.ok)
		}
	}
}
