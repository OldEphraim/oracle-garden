package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestSubscriptionsSyncForWorkflow(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		owner := mustCreateUser(t, ctx, tx)
		wfRepo := NewWorkflowsRepo()
		subs := NewSubscriptionsRepo()

		wf, err := wfRepo.Create(ctx, tx, CreateWorkflowParams{
			OwnerID: &owner, Name: "Sub test",
		})
		if err != nil {
			t.Fatalf("Create workflow: %v", err)
		}

		// Empty initial state.
		got, _ := subs.ListForWorkflow(ctx, tx, wf.ID)
		if len(got) != 0 {
			t.Errorf("initial: %d slugs", len(got))
		}

		// First sync: insert two slugs.
		if err := subs.SyncForWorkflow(ctx, tx, wf.ID, []string{"alpha", "beta"}); err != nil {
			t.Fatalf("Sync #1: %v", err)
		}
		got, _ = subs.ListForWorkflow(ctx, tx, wf.ID)
		if !equalStrings(got, []string{"alpha", "beta"}) {
			t.Errorf("after Sync #1: got %v", got)
		}

		// Second sync: replace with one new slug + one existing slug.
		// Demonstrates the delete-all-then-insert-all behavior.
		if err := subs.SyncForWorkflow(ctx, tx, wf.ID, []string{"beta", "gamma"}); err != nil {
			t.Fatalf("Sync #2: %v", err)
		}
		got, _ = subs.ListForWorkflow(ctx, tx, wf.ID)
		if !equalStrings(got, []string{"beta", "gamma"}) {
			t.Errorf("after Sync #2: got %v want [beta gamma]", got)
		}

		// Third sync: empty slice → row count goes to zero.
		if err := subs.SyncForWorkflow(ctx, tx, wf.ID, []string{}); err != nil {
			t.Fatalf("Sync #3: %v", err)
		}
		got, _ = subs.ListForWorkflow(ctx, tx, wf.ID)
		if len(got) != 0 {
			t.Errorf("after empty Sync: %v", got)
		}

		// Fourth sync: dupes within one input get deduplicated.
		if err := subs.SyncForWorkflow(ctx, tx, wf.ID, []string{"x", "x", "y", ""}); err != nil {
			t.Fatalf("Sync #4: %v", err)
		}
		got, _ = subs.ListForWorkflow(ctx, tx, wf.ID)
		if !equalStrings(got, []string{"x", "y"}) {
			t.Errorf("after dedupe Sync: got %v want [x y]", got)
		}
	})
}
