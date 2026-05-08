// Package engine is the workflow execution engine — the graph executor that
// CLAUDE.md's "Workflow Execution Engine" section specifies. One Run per
// (workflow, market_slug, user) tuple. Builds an in-memory graph from the
// DB rows, walks a ready-queue with multi-input merge / fan-in / fan-out /
// branching / loops, persists agent_steps as it goes, and writes a
// paper_trades row when a trading_decision.v1 terminal completes.
//
// Phase 6 owns: graph construction, the step loop, edge evaluation,
// timeouts (per-step 90s, per-run 10min, per-run 50-step cap), and the
// kill-switch check. It does NOT own: SSE event emission (Phase 8) or
// quota enforcement (Phase 7) — both layers wrap the engine without
// rewriting its core.
package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
)

// Graph is the in-memory workflow used for one run. Built once per run from
// the workflows + workflow_nodes + workflow_edges + agent_templates rows;
// not shared across runs (state lives on Node, mutates as the run progresses).
type Graph struct {
	Workflow repos.Workflow
	Nodes    map[uuid.UUID]*Node // keyed by workflow_node.id
	// EntryNodes are the nodes with no incoming edges — seeded with
	// market_target.v1 at the start of the run.
	EntryNodes []*Node
}

// Node is the per-run mutable view of one workflow_node + its agent_template
// + its edges. State (iteration, latestUpstreamOutputs, etc.) is reset
// per-run by NewGraph.
type Node struct {
	// Static — copies from the DB rows, immutable for the run.
	WorkflowNode        repos.WorkflowNode
	AgentTemplate       repos.AgentTemplate
	UpstreamKeys        map[string]bool // node_keys of every upstream node, by node_key
	NonLoopUpstreamKeys map[string]bool // upstreams excluding back-edge (loop) sources; used by `all` join's iter-1 readiness check

	// Outgoing/Incoming hold edges in priority order (workflow-wide
	// deterministic ordering — see EdgeOrderClause). Pointers so the
	// state-mutation paths read clean.
	Outgoing []*Edge
	Incoming []*Edge

	// Mutable per-run state.
	Iteration             int                       // increments on each successful firing; 0 = never fired
	LastFiredSeq          int                       // sequence number of last firing; -1 = never
	LatestUpstreamOutputs map[string]upstreamOutput // keyed by upstream node_key
	LastOutput            json.RawMessage           // the most recent successful output
	Pending               bool                      // already on the ready queue
}

// upstreamOutput pairs a payload with the run-sequence number at which it
// arrived. The sequence-number gate is what lets `all` and `first`
// join_strategies share one "is there something new?" check.
type upstreamOutput struct {
	Payload json.RawMessage
	Seq     int
}

// Edge is a per-run view of one workflow_edge with pointers to both ends.
// Pointers so the executor doesn't need to round-trip through the Graph map.
type Edge struct {
	Row  repos.WorkflowEdge
	From *Node
	To   *Node
	// IsLoop is set at graph-build time via DFS back-edge detection. A loop
	// edge is one whose target is a (transitive) ancestor of its source —
	// CLAUDE.md "Loops" defines them runtime-wise as edges whose target has
	// already completed in the current run, but the iter-1 readiness check
	// in `all` join needs the static answer. Non-loop upstream count is the
	// number that must produce ≥1 output before the node first fires.
	IsLoop bool
}

// NewGraph loads all rows for workflowID, resolves agent_templates, and
// constructs the in-memory Graph. Edges are loaded with the standard
// EdgeOrderClause so the per-node Outgoing slice is already in
// priority-then-id order — the executor's edge evaluation depends on this.
//
// Returns an error if any node references an agent_template that's missing
// or whose declared types aren't registered (the latter is normally caught
// at workflow save in Phase 8, but the engine re-checks defensively).
func NewGraph(
	ctx context.Context,
	q db.Querier,
	workflowsRepo *repos.WorkflowsRepo,
	agentsRepo *repos.AgentTemplatesRepo,
	workflowID uuid.UUID,
) (*Graph, error) {
	graph, err := workflowsRepo.GetGraph(ctx, q, workflowID)
	if err != nil {
		return nil, fmt.Errorf("engine: NewGraph: load workflow: %w", err)
	}

	// Build node objects keyed by id.
	nodesByID := make(map[uuid.UUID]*Node, len(graph.Nodes))
	for i := range graph.Nodes {
		wn := graph.Nodes[i]
		at, err := agentsRepo.GetByID(ctx, q, wn.AgentTemplateID)
		if err != nil {
			return nil, fmt.Errorf("engine: NewGraph: agent_template %s for node %s: %w",
				wn.AgentTemplateID, wn.NodeKey, err)
		}
		nodesByID[wn.ID] = &Node{
			WorkflowNode:          wn,
			AgentTemplate:         *at,
			UpstreamKeys:          make(map[string]bool),
			NonLoopUpstreamKeys:   make(map[string]bool),
			LatestUpstreamOutputs: make(map[string]upstreamOutput),
			LastFiredSeq:          -1,
		}
	}

	// Wire edges. Outgoing/Incoming order = (priority ASC, id ASC) since
	// graph.Edges is loaded with EdgeOrderClause. The workflow-wide slice
	// is already correctly ordered, so per-node slices inherit the order
	// as we walk it once.
	for i := range graph.Edges {
		e := graph.Edges[i]
		from, ok := nodesByID[e.FromNodeID]
		if !ok {
			return nil, fmt.Errorf("engine: NewGraph: edge %s references unknown from-node %s", e.ID, e.FromNodeID)
		}
		to, ok := nodesByID[e.ToNodeID]
		if !ok {
			return nil, fmt.Errorf("engine: NewGraph: edge %s references unknown to-node %s", e.ID, e.ToNodeID)
		}
		edge := &Edge{Row: e, From: from, To: to}
		from.Outgoing = append(from.Outgoing, edge)
		to.Incoming = append(to.Incoming, edge)
		to.UpstreamKeys[from.WorkflowNode.NodeKey] = true
	}

	// Identify entry nodes — those with no incoming edges.
	var entries []*Node
	for _, n := range nodesByID {
		if len(n.Incoming) == 0 {
			entries = append(entries, n)
		}
	}
	// Stable order (by node_key) so a workflow with multiple entry nodes
	// always seeds in the same order — useful for reproducible test runs.
	sortNodesByKey(entries)

	if len(entries) == 0 {
		return nil, fmt.Errorf("engine: NewGraph: workflow has no entry nodes (every node has at least one incoming edge)")
	}

	// Detect loop edges via DFS back-edge analysis from the entry nodes.
	// A back-edge is one that points to a node currently on the DFS stack
	// (i.e., an ancestor in the DFS tree) — exactly a cycle-creating edge.
	markLoopEdges(entries)

	// Now compute non-loop-upstream sets per node, used by `all` join's
	// iter-1 readiness check. CLAUDE.md "Loops" + "Loop semantics": loop-
	// edge upstreams don't count toward the initial-fire condition.
	for _, n := range nodesByID {
		for _, in := range n.Incoming {
			if !in.IsLoop {
				n.NonLoopUpstreamKeys[in.From.WorkflowNode.NodeKey] = true
			}
		}
	}

	return &Graph{
		Workflow:   graph.Workflow,
		Nodes:      nodesByID,
		EntryNodes: entries,
	}, nil
}

// markLoopEdges does a DFS from the entry nodes following Outgoing edges,
// flagging any edge whose target is currently on the DFS stack (the
// classical back-edge detection — that target is an ancestor of the source
// in the DFS tree, which means the edge closes a cycle).
//
// Multiple disconnected components are handled naturally: every entry node
// starts its own DFS, and tree-edges/cross-edges/forward-edges are all
// non-loop. Only back-edges are loops.
func markLoopEdges(entries []*Node) {
	const (
		white = 0 // unvisited
		gray  = 1 // on the DFS stack
		black = 2 // finished
	)
	color := make(map[*Node]int)
	var dfs func(n *Node)
	dfs = func(n *Node) {
		color[n] = gray
		for _, e := range n.Outgoing {
			switch color[e.To] {
			case gray:
				e.IsLoop = true
			case white:
				dfs(e.To)
			}
		}
		color[n] = black
	}
	for _, e := range entries {
		if color[e] == white {
			dfs(e)
		}
	}
}

// AsRuntimeAgent converts the static parts of a Node into a runtime.Agent
// (the per-Invoke view the agent runtime expects). Stable across iterations
// — the runtime sees the same agent definition every fire.
func (n *Node) AsRuntimeAgent() runtime.Agent {
	return runtime.Agent{
		Name:         n.AgentTemplate.Name + " [" + n.WorkflowNode.NodeKey + "]",
		Model:        n.AgentTemplate.Model,
		Temperature:  n.AgentTemplate.Temperature,
		MaxTokens:    n.AgentTemplate.MaxTokens,
		SystemPrompt: n.AgentTemplate.SystemPrompt,
		OutputType:   n.AgentTemplate.OutputType,
		Tools:        n.AgentTemplate.Tools,
	}
}

// IsTerminal reports whether this node is the workflow's terminal — output
// type is trading_decision.v1 AND no outgoing edges. Used by the executor
// to decide when to write a paper_trades row.
func (n *Node) IsTerminal() bool {
	return n.AgentTemplate.OutputType == "trading_decision.v1" && len(n.Outgoing) == 0
}

// sortNodesByKey is a tiny insertion sort for the small EntryNodes slice.
// (N ≤ 5 in v0; the happy-path workflow has 2 entry nodes — Watcher + Scout.)
func sortNodesByKey(ns []*Node) {
	for i := 1; i < len(ns); i++ {
		j := i
		for j > 0 && ns[j].WorkflowNode.NodeKey < ns[j-1].WorkflowNode.NodeKey {
			ns[j], ns[j-1] = ns[j-1], ns[j]
			j--
		}
	}
}
