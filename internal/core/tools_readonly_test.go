package core

import (
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
)

func TestReadOnlyToolRegistryOmitsSpawn(t *testing.T) {
	e, _ := testEngine(t)
	registry := e.toolRegistry("session-read", "", agentproto.TaskRequest{
		Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "read"},
	}, "", &instructionDiscovery{}, nil)
	for _, definition := range registry.Definitions() {
		if definition.Name == "spawn" {
			t.Fatal("read-only tool registry exposed spawn")
		}
	}
}

func TestSharedWriteToolRegistryIncludesSpawn(t *testing.T) {
	e, _ := testEngine(t)
	registry := e.toolRegistry("session-write", "", agentproto.TaskRequest{
		Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "shared-write"},
	}, "", &instructionDiscovery{}, nil)
	for _, definition := range registry.Definitions() {
		if definition.Name == "spawn" {
			return
		}
	}
	t.Fatal("shared-write tool registry omitted spawn")
}
