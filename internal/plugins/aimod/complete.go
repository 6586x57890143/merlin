package aimod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
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
// Ask is the same path for a caller that needs its answer held to a schema.
// Neither takes a temperature.

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
	return p.Ask(ctx, guildID, core.Prompt{
		System: system, User: user, MaxTokens: completeMaxTokens, Timeout: completeTimeout,
	})
}

// Ask is one call to the guild's deep model, retried once on the other
// gateway as the deep rung is. See deepPass.
func (p *Plugin) Ask(ctx context.Context, guildID string, q core.Prompt) (string, error) {
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
	if state.Exhausted || (q.Reserve > 0 && state.Spent >= state.Budget*q.Reserve) {
		return "", ErrBudgetExhausted
	}
	out, err := p.askOnce(ctx, cfg, state, q)
	if err != nil && state.FallbackKey != "" && worthFallback(err) {
		fb := state
		fb.APIKey, fb.Spec = state.FallbackKey, state.fallback()
		out, err = p.askOnce(ctx, cfg, fb, q)
	}
	var apiErr *APIError
	if errors.Is(err, errOverBudget) || (errors.As(err, &apiErr) && apiErr.Status == http.StatusPaymentRequired) {
		return "", ErrBudgetExhausted
	}
	return out, err
}

func (p *Plugin) askOnce(ctx context.Context, cfg Config, state budgetState, q core.Prompt) (string, error) {
	req := chatRequest{
		spec:     state.Spec,
		timeout:  q.Timeout,
		Models:   modelsOr(cfg.DeepModels, state.Spec.deepModels),
		Provider: strictProvider(),
		Messages: []chatMessage{
			{Role: "system", Content: q.System},
			{Role: "user", Content: q.User},
		},
		MaxTokens: q.MaxTokens,
	}
	if q.Schema != nil {
		req.ResponseFormat = &responseFormat{Type: "json_schema",
			JSONSchema: jsonSchema{Name: q.SchemaName, Strict: true, Schema: q.Schema}}
	}
	out, usage, err := p.paidChat(ctx, cfg.GuildID, state, true, req)
	p.notePayment(cfg.GuildID, err, usage.Cost > 0)
	if err != nil {
		return "", err
	}
	return out, nil
}
