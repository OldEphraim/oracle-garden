package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

func TestRunsAndStepsCRUD(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		reg := testRegistry(t)
		owner := mustCreateUser(t, ctx, tx)
		atRepo := NewAgentTemplatesRepo(reg)
		wfRepo := NewWorkflowsRepo()
		runs := NewRunsRepo()

		// Build a tiny workflow with one node so the FK on agent_steps holds.
		ag, _ := atRepo.Create(ctx, tx, CreateAgentTemplateParams{
			OwnerID: &owner, Name: "x", SystemPrompt: "x",
			InputTypes: []string{"market_target.v1"}, OutputType: "observation.v1",
		})
		wf, _ := wfRepo.Create(ctx, tx, CreateWorkflowParams{OwnerID: &owner, Name: "W"})
		node, _ := wfRepo.CreateNode(ctx, tx, CreateNodeParams{
			WorkflowID: wf.ID, AgentTemplateID: ag.ID, NodeKey: "n",
		})

		// Create a run.
		run, err := runs.Create(ctx, tx, CreateRunParams{
			WorkflowID:    wf.ID,
			UserID:        owner,
			TriggeredBy:   "manual",
			MarketSlug:    ptr("foo-slug"),
			InputSnapshot: json.RawMessage(`{"slug":"foo"}`),
		})
		if err != nil {
			t.Fatalf("Create run: %v", err)
		}
		if run.Status != "pending" {
			t.Errorf("default status: %q", run.Status)
		}

		// Add a step + finalize it.
		step, err := runs.AddStep(ctx, tx, AddStepParams{
			WorkflowRunID:  run.ID,
			WorkflowNodeID: node.ID,
			Iteration:      1,
			Status:         "running",
			InputData:      json.RawMessage(`{"market_slug":"foo"}`),
		})
		if err != nil {
			t.Fatalf("AddStep: %v", err)
		}

		if err := runs.UpdateStepCompleted(ctx, tx, UpdateStepCompletedParams{
			StepID:           step.ID,
			Status:           "completed",
			OutputData:       json.RawMessage(`{"ok":true}`),
			PromptTokens:     ptr(100),
			CompletionTokens: ptr(50),
			CostUSD:          ptr(0.0023),
			LatencyMs:        ptr(412),
		}); err != nil {
			t.Fatalf("UpdateStepCompleted: %v", err)
		}

		// Mark run completed.
		if err := runs.UpdateRunStatus(ctx, tx, run.ID, "completed", nil, true); err != nil {
			t.Fatalf("UpdateRunStatus: %v", err)
		}

		got, err := runs.GetWithSteps(ctx, tx, run.ID)
		if err != nil {
			t.Fatalf("GetWithSteps: %v", err)
		}
		if got.Run.Status != "completed" {
			t.Errorf("run status: %q", got.Run.Status)
		}
		if got.Run.FinishedAt == nil {
			t.Errorf("finished_at should be set")
		}
		if len(got.Steps) != 1 {
			t.Fatalf("steps: %d", len(got.Steps))
		}
		s := got.Steps[0]
		if s.Status != "completed" {
			t.Errorf("step status: %q", s.Status)
		}
		if s.PromptTokens == nil || *s.PromptTokens != 100 {
			t.Errorf("prompt_tokens: %v", s.PromptTokens)
		}
		if s.CostUSD == nil || *s.CostUSD < 0.002 || *s.CostUSD > 0.003 {
			t.Errorf("cost_usd: %v", s.CostUSD)
		}

		// Status updates on missing step → ErrNotFound.
		err = runs.UpdateStepStatus(ctx, tx, uuid.New(), "running")
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("UpdateStepStatus missing: want ErrNotFound, got %v", err)
		}

		// ListByWorkflow scoped to user.
		list, err := runs.ListByWorkflow(ctx, tx, wf.ID, owner, 10)
		if err != nil {
			t.Fatalf("ListByWorkflow: %v", err)
		}
		if len(list) != 1 || list[0].ID != run.ID {
			t.Errorf("ListByWorkflow: %d results", len(list))
		}
		// Different user shouldn't see this run.
		other := mustCreateUser(t, ctx, tx)
		listOther, _ := runs.ListByWorkflow(ctx, tx, wf.ID, other, 10)
		if len(listOther) != 0 {
			t.Errorf("ListByWorkflow for other user: %d results, want 0", len(listOther))
		}
	})
}
