package core

import (
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
	if e.synthesisDue(read, 3) || !e.synthesisDue(read, 4) {
		t.Fatal("a read worker synthesises at its limit")
	}
	write := agentproto.TaskRequest{Workspace: agentproto.Workspace{Mode: "shared-write"}}
	if e.synthesisDue(write, 50) {
		t.Fatal("only read workers are asked to synthesise")
	}
}
