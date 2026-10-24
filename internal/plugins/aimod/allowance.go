package aimod

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The daily cap, enforced per call rather than per batch.
//
// checkBudget answers "is there anything left today" once, and one yes used
// to admit a batch's fast call, up to twenty deep calls and their fallbacks,
// with four batches in flight at once each running four escalations. So the
// cap bounded when a guild stopped starting batches, not what it spent.
//
// Every model call now goes through paidChat, which waits for one of the
// guild's maxPaidCalls slots, reserves an allowance against the day's cap,
// makes the call, and books what it really cost before handing the
// allowance back. A call is refused when the day's spend plus everything
// in flight plus its own allowance would pass the cap.
//
// The overshoot that remains is bounded and this is it: an allowance is the
// largest single call this guild has paid for today on that rung (or the
// default below until it has paid for one), so the cap can only be passed by
// calls costing more than any before them, and by at most maxPaidCalls times
// that excess.

// maxPaidCalls bounds a guild's concurrent model calls at the HTTP boundary,
// across every batch, escalation, review and completion at once.
const maxPaidCalls = 4

// Allowances before a guild has a receipt to go on. Deliberately above what
// the default stacks charge for one call, since an allowance that is too
// small is overshoot and one that is too large only stops a few cents early.
const (
	defaultFastAllowance = 0.02
	defaultDeepAllowance = 0.10
)

// errOverBudget is a call refused because it could pass the day's cap.
// Not a failure of anything, and not worth a fallback gateway: the cap is
// the guild's own, whichever gateway would have spent it.
var errOverBudget = errors.New("aimod: the daily budget does not cover another call")

// errSpendUnbooked is a call refused because an earlier call's cost could
// not be written down. Until it is, the day's total is understated, and
// spending against an understated total is spending blind.
var errSpendUnbooked = errors.New("aimod: an earlier call's cost is not yet recorded")

// guildLedger is one guild's in-flight spend.
type guildLedger struct {
	slots chan struct{}

	mu       sync.Mutex
	reserved float64
	day      time.Time
	ceiling  [2]float64 // largest call paid for today: [fast, deep]
	unbooked []unbookedSpend
}

type unbookedSpend struct {
	day  time.Time
	u    Usage
	deep bool
}

func (p *Plugin) ledger(guildID string) *guildLedger {
	p.ledgerMu.Lock()
	defer p.ledgerMu.Unlock()
	if p.ledgers == nil {
		p.ledgers = make(map[string]*guildLedger)
	}
	l, ok := p.ledgers[guildID]
	if !ok {
		l = &guildLedger{slots: make(chan struct{}, maxPaidCalls)}
		p.ledgers[guildID] = l
	}
	return l
}

// paidChat makes one model call on the guild's account. See the top of this
// file; every call site goes through here, and none books usage itself.
func (p *Plugin) paidChat(ctx context.Context, guildID string, state budgetState, deep bool, req chatRequest) (string, Usage, error) {
	l := p.ledger(guildID)
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	}
	defer func() { <-l.slots }()

	allowance, err := p.reserve(ctx, guildID, l, state.Budget, deep)
	if err != nil {
		return "", Usage{}, err
	}
	out, usage, err := p.client.Chat(ctx, state.APIKey, req)
	p.settle(ctx, guildID, l, allowance, usage, deep)
	return out, usage, err
}

func (p *Plugin) reserve(ctx context.Context, guildID string, l *guildLedger, budget float64, deep bool) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for len(l.unbooked) > 0 {
		s := l.unbooked[0]
		if err := p.store.AddSpend(ctx, guildID, s.day, s.u, s.deep); err != nil {
			return 0, fmt.Errorf("%w: %w", errSpendUnbooked, err)
		}
		l.unbooked = l.unbooked[1:]
	}

	day := today(p.now())
	if !l.day.Equal(day) {
		l.day, l.ceiling = day, [2]float64{}
	}
	spend, err := p.store.SpendToday(ctx, guildID, day)
	if err != nil {
		// Fail closed, as checkBudget does: an unreadable total is not zero.
		return 0, fmt.Errorf("aimod: read today's spend: %w", err)
	}
	allowance := l.ceiling[rung(deep)]
	if allowance == 0 {
		allowance = defaultFastAllowance
		if deep {
			allowance = defaultDeepAllowance
		}
		// Never more than a full house of calls could share, or a guild
		// with a few cents a day would never make the first call that
		// teaches it a real figure.
		allowance = min(allowance, budget/maxPaidCalls)
	}
	if budget <= 0 || spend.SpentUSD+l.reserved+allowance > budget {
		return 0, errOverBudget
	}
	l.reserved += allowance
	return allowance, nil
}

func (p *Plugin) settle(ctx context.Context, guildID string, l *guildLedger, allowance float64, u Usage, deep bool) {
	// Detached: the call has already been paid for, and a context that ran
	// out while it was answering must not also lose the receipt.
	ctx = context.WithoutCancel(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserved -= allowance
	if u.Cost > l.ceiling[rung(deep)] {
		l.ceiling[rung(deep)] = u.Cost
	}
	// Booked whether or not the call succeeded: one that returned usage was
	// billed either way, and a budget that only counts successes is one a
	// misbehaving model walks straight through.
	if u.Cost <= 0 && u.TotalTokens <= 0 {
		return
	}
	day := today(p.now())
	if err := p.store.AddSpend(ctx, guildID, day, u, deep); err != nil {
		p.log.Error("aimod: record spend, holding paid calls until it lands", "guild", guildID, "err", err)
		l.unbooked = append(l.unbooked, unbookedSpend{day: day, u: u, deep: deep})
	}
}

func rung(deep bool) int {
	if deep {
		return 1
	}
	return 0
}
