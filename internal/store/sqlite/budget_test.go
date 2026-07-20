package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/z-chenhao/eri/internal/budget"
)

func TestModelBudgetReservesSettlesAndHardStops(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	firstTask, err := store.CreateInbound(ctx, "cli", testRef("budget-one", "hash-one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondTask, err := store.CreateInbound(ctx, "cli", testRef("budget-two", "hash-two"), nil)
	if err != nil {
		t.Fatal(err)
	}
	limits := budget.Limits{PerTask: 100, PerDay: 150, PerMonth: 1_000}
	first, err := store.ReserveModelTokens(ctx, firstTask.TaskID, 60, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveModelTokens(ctx, firstTask.TaskID, 50, limits); !errors.Is(err, budget.ErrExhausted) {
		t.Fatalf("per-task ceiling error = %v", err)
	}
	if err := store.SettleModelTokens(ctx, first, 30, true); err != nil {
		t.Fatal(err)
	}
	second, err := store.ReserveModelTokens(ctx, firstTask.TaskID, 50, limits)
	if err != nil {
		t.Fatalf("settled tokens were not released: %v", err)
	}
	if err := store.SettleModelTokens(ctx, second, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveModelTokens(ctx, secondTask.TaskID, 71, limits); !errors.Is(err, budget.ErrExhausted) {
		t.Fatalf("daily ceiling did not retain unknown reservation: %v", err)
	}
}
