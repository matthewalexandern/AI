package cognition

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/matthewalexandern/AI/internal/memory"
)

func TestAdaptiveModesDispatchBoundedPhasesAndPersistDecision(t *testing.T) {
	for _, test := range []struct {
		name, mode, input, selected string
		memory, history             bool
		phases                      []string
		budget                      int
	}{
		{"short adaptive", ModeAdaptive, "Hello!", ModeFast, false, false, []string{"answer"}, 1},
		{"analysis adaptive", ModeAdaptive, "Compare the two approaches.", ModeDeep, false, false, []string{"outline", "answer", "assessment"}, 5},
		{"multiple questions", ModeAdaptive, "Which option fits? What changes are needed?", ModeDeep, false, false, []string{"outline", "answer", "assessment"}, 5},
		{"long request", ModeAdaptive, strings.Repeat("background ", 170), ModeDeep, false, false, []string{"outline", "answer", "assessment"}, 5},
		{"medium request", ModeAdaptive, strings.Repeat("context ", 30), ModeBalanced, false, false, []string{"answer", "assessment"}, 4},
		{"recalled evidence", ModeAdaptive, "Tell me about fern.", ModeBalanced, true, false, []string{"answer", "assessment"}, 4},
		{"conversation history", ModeAdaptive, "Next question.", ModeBalanced, false, true, []string{"answer", "assessment"}, 4},
		{"explicit fast overrides complexity", ModeFast, "Compare the two approaches.", ModeFast, true, false, []string{"answer"}, 1},
		{"explicit balanced", ModeBalanced, "Hello!", ModeBalanced, false, false, []string{"answer", "assessment"}, 4},
		{"explicit deep", ModeDeep, "Hello!", ModeDeep, false, false, []string{"outline", "answer", "assessment"}, 5},
		{"legacy empty mode", "", "Hello!", ModeDeep, false, false, []string{"outline", "answer", "assessment"}, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t)
			if test.memory {
				if _, err := store.Remember(context.Background(), "A fern prefers indirect sunlight. Compare the two approaches using this evidence.", "explicit"); err != nil {
					t.Fatal(err)
				}
			}
			if test.history {
				if err := store.SaveTurn(context.Background(), memory.Turn{Session: "adaptive", User: "Earlier request", Answer: "Earlier response"}); err != nil {
					t.Fatal(err)
				}
			}
			var phases []string
			generator := generatorFunc(func(_ context.Context, messages []memory.Message, _ float64) (string, error) {
				_, after, ok := strings.Cut(messages[0].Content, "PHASE: ")
				if !ok {
					t.Fatal("missing observable phase")
				}
				phase, _, _ := strings.Cut(after, "\n")
				phases = append(phases, phase)
				return responseForPhase(messages), nil
			})
			result, err := New(generator, store, Config{Mode: test.mode}).Run(context.Background(), "adaptive", test.input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(phases, test.phases) || !reflect.DeepEqual(result.Decision.Phases, test.phases) || result.Decision.Calls != len(test.phases) || result.Decision.CallBudget != test.budget || result.Decision.Mode != test.selected {
				t.Fatalf("dispatch = %+v, phases=%v; want mode=%s, phases=%v, budget=%d", result.Decision, phases, test.selected, test.phases, test.budget)
			}
			requested := test.mode
			if requested == "" {
				requested = ModeDeep
			}
			if result.Decision.RequestedMode != requested || result.Decision.Reason == "" || result.Decision.DurationMS < 0 {
				t.Fatalf("missing scheduling metadata: %+v", result.Decision)
			}
			if result.Assessed != (test.selected != ModeFast) || (result.Plan != "") != (test.selected == ModeDeep) {
				t.Fatalf("incorrect outline/assessment status: %+v", result)
			}
			if !result.Assessed && (result.Assessment.Confidence != 0 || result.Assessment.NeedsRevision || result.Assessment.Missing == nil || len(result.Assessment.Missing) != 0 || !strings.Contains(result.Assessment.Notes, "Not assessed")) {
				t.Fatalf("fast result claims model assessment: %+v", result.Assessment)
			}
			if test.memory && len(result.Memories) == 0 {
				t.Fatal("mode selection lost recalled evidence")
			}
			turns, err := store.Episodes(context.Background(), "adaptive", 10)
			if err != nil || len(turns) == 0 {
				t.Fatalf("read persisted decision: %+v, %v", turns, err)
			}
			last := turns[len(turns)-1]
			var saved struct {
				Assessed bool     `json:"assessed"`
				Decision Decision `json:"decision"`
			}
			if err := json.Unmarshal([]byte(last.Cognition), &saved); err != nil || saved.Assessed != result.Assessed || !reflect.DeepEqual(saved.Decision, result.Decision) {
				t.Fatalf("persisted decision mismatch: %+v, %v", saved, err)
			}
			var reflection map[string]any
			if err := json.Unmarshal([]byte(last.Reflection), &reflection); err != nil || len(reflection) != 4 {
				t.Fatalf("legacy assessment shape changed: %s, %v", last.Reflection, err)
			}
		})
	}
}

func TestBalancedRevisionRemainsWithinFourCalls(t *testing.T) {
	store := newStore(t)
	generator := &scriptedGenerator{responses: []generated{
		{text: "Original answer"}, {text: assessmentJSON(0.99, true)},
		{text: "Corrected answer"}, {text: assessmentJSON(0.01, true)},
	}}
	result, err := New(generator, store, Config{Mode: ModeBalanced}).Run(context.Background(), "balanced-revision", "Answer this request")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Revised || !result.Assessed || result.Decision.Calls != 4 || result.Decision.CallBudget != 4 || len(generator.calls) != 4 || result.Assessment.Confidence != .01 || !result.Assessment.NeedsRevision {
		t.Fatalf("balanced revision exceeded its budget or lost final assessment: %+v", result)
	}
	if !reflect.DeepEqual(result.Decision.Phases, []string{"answer", "assessment", "revision", "assessment"}) {
		t.Fatalf("incorrect revision phases: %v", result.Decision.Phases)
	}
}

func TestConfidenceNeverSelectsExtraInference(t *testing.T) {
	for _, confidence := range []float64{0, 1} {
		store := newStore(t)
		generator := &scriptedGenerator{responses: []generated{{text: "Answer"}, {text: assessmentJSON(confidence, false)}}}
		result, err := New(generator, store, Config{Mode: ModeBalanced}).Run(context.Background(), "advisory", "A request")
		if err != nil || result.Decision.Calls != 2 || result.Revised || len(generator.calls) != 2 {
			t.Fatalf("confidence %g changed call scheduling: %+v, %v", confidence, result, err)
		}
	}
}

func TestAdaptiveUsesHistoryFromPreviousCompletedTurn(t *testing.T) {
	store := newStore(t)
	generator := generatorFunc(func(_ context.Context, messages []memory.Message, _ float64) (string, error) {
		return responseForPhase(messages), nil
	})
	controller := New(generator, store, Config{Mode: ModeAdaptive})
	first, err := controller.Run(context.Background(), "conversation", "Hello!")
	if err != nil || first.Decision.Mode != ModeFast {
		t.Fatalf("first turn = %+v, %v", first, err)
	}
	second, err := controller.Run(context.Background(), "conversation", "Next please.")
	if err != nil || second.Decision.Mode != ModeBalanced || second.Decision.Calls != 2 {
		t.Fatalf("follow-up did not adapt to available history: %+v, %v", second, err)
	}
}

func TestEveryModeCancellationLeavesNoPartialTurn(t *testing.T) {
	for _, mode := range []string{ModeAdaptive, ModeFast, ModeBalanced, ModeDeep} {
		t.Run(mode, func(t *testing.T) {
			store := newStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			generator := generatorFunc(func(_ context.Context, _ []memory.Message, _ float64) (string, error) {
				cancel()
				return "This generated fragment must not be stored", nil
			})
			if _, err := New(generator, store, Config{Mode: mode}).Run(ctx, "cancelled", "Hello"); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was not preserved: %v", err)
			}
			assertEmptySession(t, store, "cancelled")
		})
	}
}

func TestInvalidModeFailsBeforeInferenceOrPersistence(t *testing.T) {
	store := newStore(t)
	generator := &scriptedGenerator{}
	if _, err := New(generator, store, Config{Mode: "automatic"}).Run(context.Background(), "invalid-mode", "Hello"); err == nil || !strings.Contains(err.Error(), "unsupported mode") {
		t.Fatalf("invalid mode error = %v", err)
	}
	if len(generator.calls) != 0 {
		t.Fatal("invalid mode invoked inference")
	}
	assertEmptySession(t, store, "invalid-mode")
}

func TestFastAndBalancedFailuresNeverCommitPartialTurns(t *testing.T) {
	failure := errors.New("fixture phase failure")
	for _, mode := range []string{ModeFast, ModeBalanced} {
		responses := []generated{{text: "Draft"}}
		if mode == ModeBalanced {
			responses = append(responses, generated{text: assessmentJSON(.7, true)}, generated{text: "Revision"}, generated{text: assessmentJSON(.4, false)})
		}
		for failing := range responses {
			store := newStore(t)
			inference := append([]generated(nil), responses...)
			inference[failing] = generated{err: failure}
			generator := &scriptedGenerator{responses: inference}
			if _, err := New(generator, store, Config{Mode: mode}).Run(context.Background(), "phase-failure", "User request"); !errors.Is(err, failure) {
				t.Fatalf("%s failure at phase %d returned %v", mode, failing, err)
			}
			if len(generator.calls) != failing+1 {
				t.Fatalf("%s retried or continued after phase %d", mode, failing)
			}
			assertEmptySession(t, store, "phase-failure")
		}
	}
	store := newStore(t)
	generator := &scriptedGenerator{responses: []generated{{text: "Draft"}, {text: `{"confidence":0.7}`}}}
	if _, err := New(generator, store, Config{Mode: ModeBalanced}).Run(context.Background(), "invalid-assessment", "User request"); err == nil {
		t.Fatal("balanced mode accepted malformed assessment")
	}
	assertEmptySession(t, store, "invalid-assessment")
}
