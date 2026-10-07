package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matthewalexandern/AI/internal/cognition"
	"github.com/matthewalexandern/AI/internal/memory"
)

type waitingGenerator struct{}

func (waitingGenerator) Complete(ctx context.Context, _ []memory.Message, _ float64) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestTurnBudgetCancelsInferenceWithoutPersisting(t *testing.T) {
	store, err := openMemory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller := cognition.New(waitingGenerator{}, store, cognition.Config{Mode: "fast"})
	ctx := context.WithValue(t.Context(), turnTimeoutKey{}, 20*time.Millisecond)
	if _, err := turn(ctx, controller, "bounded", "Hello"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline did not cancel inference: %v", err)
	}
	episodes, err := store.Episodes(t.Context(), "bounded", 10)
	if err != nil || len(episodes) != 0 {
		t.Fatalf("canceled turn persisted: %v / %v", episodes, err)
	}
}

func TestInvalidTurnTimeoutFailsBeforeModelLoading(t *testing.T) {
	for _, timeout := range []string{"9s", "61m"} {
		if _, err := runCLI(t, t.TempDir(), "ask", "--turn-timeout", timeout, "Hello"); err == nil {
			t.Fatalf("accepted invalid timeout %s", timeout)
		}
	}
}
