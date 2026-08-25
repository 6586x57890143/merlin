package aimod

import (
	"context"
	"fmt"
	"time"
)

// today is the UTC date key every spend row is filed under.
//
// UTC rather than a guild's local time, matching OpenRouter's own daily
// counters, so "you have spent $0.31 of $0.50 today" means the same thing on
// both sides of the account. A guild whose day rolls at an awkward hour is a
// smaller problem than two systems disagreeing about when the day was.
func today(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// budgetState is what the ladder needs to know before spending anything.
type budgetState struct {
	APIKey    string
	Spent     float64
	Budget    float64
	Exhausted bool
}

// checkBudget resolves the guild's key and remaining allowance.
//
// A guild over its cap is not an error and must not be logged as one: it is
// an admin's deliberate ceiling doing exactly what it was set to do. The
// caller degrades to the free rungs and posts one audit entry per day, the
// same reasoning discordguard.Skipped exists for.
func (p *Plugin) checkBudget(ctx context.Context, cfg Config) (budgetState, error) {
	if len(cfg.APIKeySealed) == 0 {
		return budgetState{Exhausted: true}, nil
	}
	key, err := p.sealer.open(cfg.APIKeySealed)
	if err != nil {
		return budgetState{Exhausted: true}, err
	}
	spend, err := p.store.SpendToday(ctx, cfg.GuildID, today(p.now()))
	if err != nil {
		// Fail closed on an unreadable spend row. Assuming zero spent would
		// make an unreachable database into an uncapped one, which is the
		// single worst direction to be wrong about somebody else's money.
		return budgetState{Exhausted: true}, err
	}
	return budgetState{
		APIKey:    key,
		Spent:     spend.SpentUSD,
		Budget:    cfg.DailyBudgetUSD,
		Exhausted: spend.SpentUSD >= cfg.DailyBudgetUSD,
	}, nil
}

// recordUsage books one call against the guild's day.
//
// Called for every response that carried a usage block, including ones whose
// body failed to parse: the money left the account either way, and a budget
// that only counts successful calls is a budget a misbehaving model can walk
// straight through.
func (p *Plugin) recordUsage(ctx context.Context, guildID string, u Usage, deep bool) {
	if err := p.store.AddSpend(ctx, guildID, today(p.now()), u, deep); err != nil {
		p.log.Error("aimod: record spend", "guild", guildID, "err", err)
	}
}

// Estimate is a projected daily cost for one model stack, built from this
// guild's own measured traffic rather than a guess.
//
// The two halves come from different places on purpose. Tokens per call is
// measured here, because only this bot knows how long its prompts and this
// server's messages actually are. Price per token comes from OpenRouter,
// because only they know what a model costs today. Multiplying a guess by a
// guess would produce a number nobody should act on, and this is a number an
// admin is going to set a budget from.
type Estimate struct {
	// Basis says where the numbers came from, and is shown to the admin.
	// An estimate with no measured traffic behind it says so rather than
	// quietly presenting a default as a measurement.
	Basis            string
	Measured         bool
	ScannedPerDay    float64
	FastTokensPerMsg float64
	DeepTokensPerMsg float64
	// DeepRate is the share of scanned messages that reached the deep tier.
	DeepRate float64
	// USDPerDay is the projection for the stack it was computed against.
	USDPerDay float64
}

// assumedScannedPerDay and the token figures below are only used when a
// guild has no measured history yet, which is every guild on its first day.
// They are openly labelled as assumptions in the output.
const (
	assumedScannedPerDay    = 2000
	assumedFastTokensPerMsg = 60
	assumedDeepTokensPerMsg = 1600
	assumedDeepRate         = 0.01
)

// estimateFor projects a day's cost for one fast model and one deep model.
//
// Completion tokens are folded in at the measured ratio rather than priced
// separately per tier, because for a classifier they are a rounding error
// against the prompt: the fast pass answers with an empty array most of the
// time. They are still counted, so the estimate does not drift if a future
// model starts reasoning at length.
func estimateFor(history []Spend, fast, deep Model) Estimate {
	est := Estimate{
		Basis:            "assumed traffic, no measured history yet",
		ScannedPerDay:    assumedScannedPerDay,
		FastTokensPerMsg: assumedFastTokensPerMsg,
		DeepTokensPerMsg: assumedDeepTokensPerMsg,
		DeepRate:         assumedDeepRate,
	}

	var days, scanned, fastCalls, deepCalls float64
	var fastPrompt, fastCompletion, deepPrompt, deepCompletion float64
	for _, s := range history {
		if s.Scanned == 0 && s.FastCalls == 0 {
			continue
		}
		days++
		scanned += float64(s.Scanned)
		fastCalls += float64(s.FastCalls)
		deepCalls += float64(s.DeepCalls)
		fastPrompt += float64(s.FastPromptTokens)
		fastCompletion += float64(s.FastCompletionTokens)
		deepPrompt += float64(s.DeepPromptTokens)
		deepCompletion += float64(s.DeepCompletionTokens)
	}

	if days > 0 && scanned > 0 {
		est.Measured = true
		est.Basis = fmt.Sprintf("measured over %.0f day(s) of this server's own traffic", days)
		est.ScannedPerDay = scanned / days
		est.FastTokensPerMsg = (fastPrompt + fastCompletion) / scanned
		est.DeepRate = deepCalls / scanned
		if deepCalls > 0 {
			est.DeepTokensPerMsg = (deepPrompt + deepCompletion) / deepCalls
		} else {
			est.DeepTokensPerMsg = assumedDeepTokensPerMsg
		}
	}

	// Prices are quoted per million tokens; blended is a single rate per
	// tier, since the split between prompt and completion is already baked
	// into the measured tokens-per-message above.
	fastCost := est.ScannedPerDay * est.FastTokensPerMsg * blendedPerToken(fast)
	deepCost := est.ScannedPerDay * est.DeepRate * est.DeepTokensPerMsg * blendedPerToken(deep)
	est.USDPerDay = fastCost + deepCost
	return est
}

// blendedPerToken weights a model's two rates the way a classifier actually
// uses them: almost all prompt, a little completion.
func blendedPerToken(m Model) float64 {
	const completionShare = 0.05
	return ((m.PromptPerM * (1 - completionShare)) + (m.CompletionPerM * completionShare)) / 1e6
}

// formatUSD renders money at a precision that stays honest at both ends. A
// nano model on a quiet server costs fractions of a cent a day, and printing
// that as "$0.00" would tell an admin the feature is free.
func formatUSD(v float64) string {
	switch {
	case v == 0:
		return "$0.00"
	case v < 0.01:
		return fmt.Sprintf("$%.5f", v)
	case v < 1:
		return fmt.Sprintf("$%.4f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}
