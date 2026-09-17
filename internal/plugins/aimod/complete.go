package aimod

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Complete: one free-text model call, for another plugin.
//
// The rapsheet plugin wants a model for three things (a narrative of one
// member's record, a weekly look for inconsistent sentencing, a second
// opinion on a possible alt) and none of them should come with a second
// gateway key, a second budget or a second set of provider rules. So this
// is the same request path calibration uses, with the guild's key, booked
// against the guild's daily cap, with the same ZDR and provider preferences,
// exposed as one method that takes a system and a user message and returns
// text. Wired in cmd/bot/main.go through a narrow interface on the caller's
// side; this package still imports nothing of theirs.
//
// What it will not do is take a schema or a temperature. A caller wanting
// structured output is a classifier, and classifiers live in this package.

// unavailableError is what Complete returns when the guild has no way to
// pay for the call. It is not a failure of the request, and a caller should
// degrade to whatever it does without a model rather than retry or alert.
// The Unavailable method is the contract: a caller checks it through an
// anonymous interface and needs no import of this package.
type unavailableError struct{ msg string }

func (e unavailableError) Error() string     { return e.msg }
func (e unavailableError) Unavailable() bool { return true }

var (
	// ErrNoKey means the guild has no gateway key, or AI moderation is
	// disabled there.
	ErrNoKey error = unavailableError{"aimod: no gateway key configured for this guild"}
	// ErrBudgetExhausted means the guild's daily cap is spent.
	ErrBudgetExhausted error = unavailableError{"aimod: the daily model budget for this guild is spent"}
)

const (
	// completeMaxTokens bounds one answer. A narrative or a review that
	// needs more than this needs an editor, not a bigger budget.
	completeMaxTokens = 800
	// completeTimeout is the deep model on a long prompt, plus headroom.
	completeTimeout = 90 * time.Second
)

// Complete answers user under system with the guild's deep model, booking
// the cost against the guild's daily budget whether or not the body parses.
func (p *Plugin) Complete(ctx context.Context, guildID, system, user string) (string, error) {
	if p.gate != nil && !p.gate.PluginEnabled(guildID, p.Name()) {
		return "", ErrNoKey
	}
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return "", fmt.Errorf("aimod: read config: %w", err)
	}
	if _, sealed := route(cfg); len(sealed) == 0 {
		return "", ErrNoKey
	}
	state, err := p.checkBudget(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("aimod: check budget: %w", err)
	}
	if state.Exhausted {
		return "", ErrBudgetExhausted
	}
	out, usage, err := p.client.Chat(ctx, state.APIKey, chatRequest{
		spec:     state.Spec,
		timeout:  completeTimeout,
		Models:   modelsOr(cfg.DeepModels, state.Spec.deepModels),
		Provider: strictProvider(),
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: completeMaxTokens,
	})
	// Booked before the error is handled, matching classify and calibrate:
	// a call that returned usage was billed whether or not it answered.
	if usage.Cost > 0 || usage.TotalTokens > 0 {
		p.recordUsage(ctx, guildID, usage, true)
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 402 {
			return "", ErrBudgetExhausted
		}
		return "", err
	}
	return out, nil
}
