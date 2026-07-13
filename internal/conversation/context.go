package conversation

import (
	"context"

	"hify/internal/agent"
	"hify/internal/provider"
)

const (
	// maxContextFetchMessages bounds the SQL side of the two-layer
	// truncation: never pull more than this many rows regardless of how
	// long the conversation has gotten.
	maxContextFetchMessages = 200

	// charsPerToken is the no-dependency token-budget heuristic (see
	// CLAUDE.md) — good enough to keep requests under a model's context
	// window without pulling in a tokenizer.
	charsPerToken = 4

	// defaultContextBudgetTokens is used when a model has no configured
	// context_window.
	defaultContextBudgetTokens = 4000

	// outputReserveTokens is held back from the model's context window for
	// the reply itself, so a full-context prompt doesn't leave no room for
	// the model to answer.
	outputReserveTokens = 1000

	minContextBudgetTokens = 500
)

// assembleContext builds the message list for one ChatStream call: the
// agent's system prompt (if any) followed by as much recent history as
// fits the token budget, oldest kept message first.
func (s *service) assembleContext(ctx context.Context, conversationID string, ag agent.Agent, model provider.Model) ([]provider.Message, error) {
	rows, err := s.repo.listRecentMessages(ctx, conversationID, maxContextFetchMessages)
	if err != nil {
		return nil, err
	}
	reverseMessages(rows) // DB gives newest-first; we want chronological order

	budgetChars := contextBudgetChars(model, ag.SystemPrompt)
	kept := truncateByBudget(rows, budgetChars)

	out := make([]provider.Message, 0, len(kept)+1)
	if ag.SystemPrompt != "" {
		out = append(out, provider.Message{Role: provider.RoleSystem, Content: ag.SystemPrompt})
	}
	for _, m := range kept {
		out = append(out, provider.Message{Role: provider.Role(m.Role), Content: m.Content})
	}
	return out, nil
}

func contextBudgetChars(model provider.Model, systemPrompt string) int {
	budgetTokens := defaultContextBudgetTokens
	if model.ContextWindow != nil {
		budgetTokens = *model.ContextWindow - outputReserveTokens
		if budgetTokens < minContextBudgetTokens {
			budgetTokens = minContextBudgetTokens
		}
	}
	budgetChars := budgetTokens * charsPerToken
	budgetChars -= len(systemPrompt)
	return budgetChars
}

// truncateByBudget drops the oldest messages (front of a chronologically
// ordered slice) until the remainder's total content length fits within
// budgetChars — always keeping at least the most recent message, since
// that's the one the user just sent.
func truncateByBudget(rows []Message, budgetChars int) []Message {
	if len(rows) == 0 {
		return rows
	}
	if budgetChars <= 0 {
		return rows[len(rows)-1:]
	}

	total := 0
	cut := 0
	for i := len(rows) - 1; i >= 0; i-- {
		total += len(rows[i].Content)
		if total > budgetChars {
			cut = i + 1
			break
		}
	}
	if cut >= len(rows) {
		return rows[len(rows)-1:]
	}
	return rows[cut:]
}

func reverseMessages(rows []Message) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}
