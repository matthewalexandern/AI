package cognition

import (
	"strings"
	"unicode"
)

const (
	ModeAdaptive = "adaptive"
	ModeFast     = "fast"
	ModeBalanced = "balanced"
	ModeDeep     = "deep"
)

// Decision records observable scheduling facts, never private model reasoning.
// DurationMS includes queueing, recall, and generation before the final database
// commit. The same decision is returned to callers and stored with the episode.
type Decision struct {
	RequestedMode string   `json:"requested_mode"`
	Mode          string   `json:"mode"`
	Reason        string   `json:"reason"`
	CallBudget    int      `json:"call_budget"`
	Calls         int      `json:"calls"`
	Phases        []string `json:"phases"`
	DurationMS    int64    `json:"duration_ms"`
}

func validMode(mode string) bool {
	switch mode {
	case ModeAdaptive, ModeFast, ModeBalanced, ModeDeep:
		return true
	default:
		return false
	}
}

// chooseMode uses bounded, deterministic input and context features. This is a
// scheduling heuristic, not a model confidence score or a claim of difficulty.
func chooseMode(requested, input string, memories, historyMessages int) Decision {
	mode, reason := requested, "Explicit mode selection."
	if requested == ModeAdaptive {
		words := strings.Fields(input)
		tokens := strings.FieldsFunc(strings.ToLower(input), func(r rune) bool { return !unicode.IsLetter(r) })
		analysis := false
		for _, word := range tokens {
			switch word {
			case "analyze", "analyse", "compare", "debug", "design", "evaluate", "investigate", "critique", "architecture", "tradeoff", "tradeoffs", "review", "prove":
				analysis = true
			}
		}
		nonemptyLines := 0
		for _, line := range strings.Split(input, "\n") {
			if strings.TrimSpace(line) != "" {
				nonemptyLines++
			}
		}
		switch {
		case analysis:
			mode, reason = ModeDeep, "Request includes an analysis, comparison, design, or review task."
		case strings.Count(input, "?") >= 2 || nonemptyLines >= 3:
			mode, reason = ModeDeep, "Request contains multiple questions or several input lines."
		case len(input) > 1200 || len(words) > 160:
			mode, reason = ModeDeep, "Long request benefits from a task outline and assessment."
		case memories > 0 || historyMessages > 0:
			mode, reason = ModeBalanced, "Retrieved evidence or prior conversation is available."
		case len(input) <= 160 && len(words) <= 24:
			mode, reason = ModeFast, "Short request with no retrieved evidence or prior conversation."
		default:
			mode, reason = ModeBalanced, "Request exceeds the short-answer threshold."
		}
	}
	budget := map[string]int{ModeFast: 1, ModeBalanced: 4, ModeDeep: 5}[mode]
	return Decision{RequestedMode: requested, Mode: mode, Reason: reason, CallBudget: budget, Phases: []string{}}
}
