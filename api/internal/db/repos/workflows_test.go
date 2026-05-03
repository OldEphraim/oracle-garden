package repos

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestWorkflowsCRUD covers create/get/update/delete on the workflow row,
// plus listing nodes and edges. The edge-priority invariant has its own
// dedicated test below.
func TestWorkflowsCRUD(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		reg := testRegistry(t)
		owner := mustCreateUser(t, ctx, tx)
		atRepo := NewAgentTemplatesRepo(reg)
		wfRepo := NewWorkflowsRepo()

		// Need at least two agent templates to wire as nodes.
		makeAgent := func(name, output string) uuid.UUID {
			a, err := atRepo.Create(ctx, tx, CreateAgentTemplateParams{
				OwnerID: &owner, Name: name, SystemPrompt: "x",
				InputTypes: []string{"market_target.v1"}, OutputType: output,
			})
			if err != nil {
				t.Fatalf("create agent %s: %v", name, err)
			}
			return a.ID
		}
		watcher := makeAgent("Watcher", "observation.v1")
		thesis := makeAgent("Thesis", "thesis.v1")

		// Create workflow
		wf, err := wfRepo.Create(ctx, tx, CreateWorkflowParams{
			OwnerID:       &owner,
			Name:          "Test",
			MarketTargets: []string{"foo", "bar"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !wf.IsActive {
			t.Errorf("default is_active should be true")
		}
		if wf.Visibility != "private" {
			t.Errorf("default visibility: got %q", wf.Visibility)
		}

		// Add nodes
		n1, err := wfRepo.CreateNode(ctx, tx, CreateNodeParams{
			WorkflowID: wf.ID, AgentTemplateID: watcher, NodeKey: "watcher",
		})
		if err != nil {
			t.Fatalf("CreateNode watcher: %v", err)
		}
		if n1.JoinStrategy != "all" {
			t.Errorf("default join_strategy: %q", n1.JoinStrategy)
		}
		if n1.LoopIterationLimit != 5 {
			t.Errorf("default loop_iteration_limit: %d", n1.LoopIterationLimit)
		}
		n2, err := wfRepo.CreateNode(ctx, tx, CreateNodeParams{
			WorkflowID: wf.ID, AgentTemplateID: thesis, NodeKey: "thesis",
		})
		if err != nil {
			t.Fatalf("CreateNode thesis: %v", err)
		}

		// Update workflow
		newName := "Renamed"
		updated, err := wfRepo.Update(ctx, tx, wf.ID, UpdateWorkflowParams{Name: &newName})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.Name != "Renamed" {
			t.Errorf("name: %q", updated.Name)
		}

		// GetGraph: workflow + 2 nodes + 0 edges so far.
		g, err := wfRepo.GetGraph(ctx, tx, wf.ID)
		if err != nil {
			t.Fatalf("GetGraph: %v", err)
		}
		if len(g.Nodes) != 2 {
			t.Errorf("nodes: got %d want 2", len(g.Nodes))
		}
		if len(g.Edges) != 0 {
			t.Errorf("edges: got %d want 0", len(g.Edges))
		}

		// One edge, then re-fetch
		_, err = wfRepo.CreateEdge(ctx, tx, CreateEdgeParams{
			WorkflowID: wf.ID, FromNodeID: n1.ID, ToNodeID: n2.ID,
			Condition: "always", Priority: 0,
		})
		if err != nil {
			t.Fatalf("CreateEdge: %v", err)
		}
		g2, _ := wfRepo.GetGraph(ctx, tx, wf.ID)
		if len(g2.Edges) != 1 {
			t.Errorf("edges after insert: %d", len(g2.Edges))
		}

		// ListByOwner
		owned, err := wfRepo.ListByOwner(ctx, tx, owner, false, 10)
		if err != nil {
			t.Fatalf("ListByOwner: %v", err)
		}
		if len(owned) != 1 || owned[0].ID != wf.ID {
			t.Errorf("ListByOwner: %d results", len(owned))
		}
	})
}

// TestWorkflowEdgesPriorityOrder is the load-bearing test for STEPS.md /
// CLAUDE.md's non-negotiable: edges fetched for a node MUST come back in
// (priority ASC, id ASC) order. The engine's conditional-edge evaluation
// relies on this; the engine tests in Phase 6 will go silently flaky if
// this contract breaks.
func TestWorkflowEdgesPriorityOrder(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		reg := testRegistry(t)
		owner := mustCreateUser(t, ctx, tx)
		atRepo := NewAgentTemplatesRepo(reg)
		wfRepo := NewWorkflowsRepo()

		mkAgent := func(name, out string) uuid.UUID {
			a, err := atRepo.Create(ctx, tx, CreateAgentTemplateParams{
				OwnerID: &owner, Name: name, SystemPrompt: "x",
				InputTypes: []string{"market_target.v1"}, OutputType: out,
			})
			if err != nil {
				t.Fatal(err)
			}
			return a.ID
		}
		src := mkAgent("Src", "observation.v1")
		dst := mkAgent("Dst", "thesis.v1")

		wf, _ := wfRepo.Create(ctx, tx, CreateWorkflowParams{
			OwnerID: &owner, Name: "P",
		})
		nSrc, _ := wfRepo.CreateNode(ctx, tx, CreateNodeParams{
			WorkflowID: wf.ID, AgentTemplateID: src, NodeKey: "s",
		})
		nDst, _ := wfRepo.CreateNode(ctx, tx, CreateNodeParams{
			WorkflowID: wf.ID, AgentTemplateID: dst, NodeKey: "d",
		})

		// Insert edges in non-monotonic order; the repo MUST return them
		// in priority ASC.
		insertOrder := []int{2, 0, 5, 1, 3}
		for _, p := range insertOrder {
			if _, err := wfRepo.CreateEdge(ctx, tx, CreateEdgeParams{
				WorkflowID: wf.ID, FromNodeID: nSrc.ID, ToNodeID: nDst.ID,
				Condition: "always", Priority: p,
			}); err != nil {
				t.Fatalf("CreateEdge p=%d: %v", p, err)
			}
		}

		check := func(label string, edges []WorkflowEdge) {
			t.Helper()
			want := []int{0, 1, 2, 3, 5}
			if len(edges) != len(want) {
				t.Fatalf("%s: got %d edges, want %d", label, len(edges), len(want))
			}
			for i, e := range edges {
				if e.Priority != want[i] {
					t.Errorf("%s: edges[%d].Priority = %d, want %d", label, i, e.Priority, want[i])
				}
			}
		}

		// ListEdges (workflow-wide) must be ordered.
		all, err := wfRepo.ListEdges(ctx, tx, wf.ID)
		if err != nil {
			t.Fatalf("ListEdges: %v", err)
		}
		check("ListEdges", all)

		// ListEdgesFromNode (per-node) must use the same ordering — engine
		// uses this during traversal.
		fromNode, err := wfRepo.ListEdgesFromNode(ctx, tx, nSrc.ID)
		if err != nil {
			t.Fatalf("ListEdgesFromNode: %v", err)
		}
		check("ListEdgesFromNode", fromNode)

		// GetGraph composes ListEdges; same invariant.
		g, _ := wfRepo.GetGraph(ctx, tx, wf.ID)
		check("GetGraph.Edges", g.Edges)
	})
}
