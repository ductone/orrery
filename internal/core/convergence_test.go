package core

import (
	"context"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
)

func TestWorkerTurnLimit(t *testing.T) {
	read := agentproto.TaskRequest{Workspace: agentproto.Workspace{Mode: "read"}}
	if workerTurnLimit(read) != defaultWorkerTurns {
		t.Fatal("default limit")
	}
	planned := read
	planned.Hints.WorkerTurns = 10
	if workerTurnLimit(planned) != 10 {
		t.Fatal("a spawner's limit wins")
	}
	e, _ := testEngine(t)
	ctx := context.Background()
	if e.synthesisDue(ctx, "s", "t", read, 3, nil, nil) || !e.synthesisDue(ctx, "s", "t", read, 4, nil, nil) {
		t.Fatal("a read worker synthesises at its limit")
	}
	write := agentproto.TaskRequest{Workspace: agentproto.Workspace{Mode: "shared-write"}}
	if e.synthesisDue(ctx, "s", "t", write, 50, nil, nil) {
		t.Fatal("only read workers are asked to synthesise")
	}
}
