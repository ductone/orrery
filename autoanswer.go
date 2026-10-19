package orrery

import (
	"context"

	"github.com/ductone/orrery/agentproto"
)

// AutoAnswer returns an InputHandler for unattended agents: it agrees to
// extend the budget until the session's total budget would exceed
// maxSessionUSD (then stops the session by choosing the stop option), answers
// ask questions with askReply (pausing if askReply is empty), and lets limit
// questions continue. A choice-only ask question that askReply is not one of
// is rejected by the engine and so pauses. Bound the number of answers per
// run with Options.MaxAutoAnswers.
func AutoAnswer(maxSessionUSD float64, askReply string) InputHandler {
	return func(_ context.Context, _ string, input agentproto.InputRequest) (string, bool, error) {
		switch input.Kind {
		case agentproto.InputAsk:
			return askReply, askReply != "", nil
		case agentproto.InputBudget:
			if input.BudgetUSD+input.ExtensionUSD > maxSessionUSD {
				return agentproto.ChoiceStop, true, nil
			}
			for _, choice := range input.Choices {
				if choice != agentproto.ChoiceStop {
					return choice, true, nil
				}
			}
			return "", false, nil
		case agentproto.InputLimit:
			return agentproto.ChoiceContinue, true, nil
		}
		return "", false, nil
	}
}
