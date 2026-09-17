package aimod

import (
	"context"
	"errors"
	"testing"

	"github.com/6586x57890143/merlin/internal/secret"
)

// completePlugin is testPlugin with a real sealer and a sealed key in the
// guild's config, since Complete has to open the key to run.
func completePlugin(t *testing.T, store *fakeStore, cfg Config, chat classifier) *Plugin {
	t.Helper()
	p := testPlugin(t, store, chat, newFakeOps(), &fakeAudit{})
	sealer, err := secret.New(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	p.sealer = sealer
	if cfg.APIKeySealed != nil {
		sealed, err := sealer.Seal("sk-or-v1-test")
		if err != nil {
			t.Fatal(err)
		}
		cfg.APIKeySealed = sealed
	}
	store.setConfig(cfg)
	return p
}

// scriptedChat answers every request the same way and remembers the last
// one, which is all a test of Complete needs to see.
type scriptedChat struct {
	out   string
	usage Usage
	err   error
	last  chatRequest
	calls int
}

func (s *scriptedChat) Chat(_ context.Context, _ string, req chatRequest) (string, Usage, error) {
	s.calls++
	s.last = req
	return s.out, s.usage, s.err
}

func TestCompleteRunsOnTheGuildsKeyAndBooksTheSpend(t *testing.T) {
	store := newFakeStore()
	chat := &scriptedChat{out: "a paragraph", usage: Usage{Cost: 0.002, TotalTokens: 400}}
	p := completePlugin(t, store, enforcingConfig(), chat)

	out, err := p.Complete(context.Background(), "g1", "be brief", "summarise this")
	if err != nil || out != "a paragraph" {
		t.Fatalf("Complete = %q, %v", out, err)
	}
	if chat.last.ResponseFormat != nil || chat.last.Temperature != 0 || chat.last.Provider == nil || !chat.last.Provider.ZDR {
		t.Errorf("request = %+v; want no schema, temperature 0, strict provider", chat.last)
	}
	if len(chat.last.Messages) != 2 || chat.last.Messages[0].Content != "be brief" || chat.last.Messages[1].Content != "summarise this" {
		t.Errorf("messages = %+v", chat.last.Messages)
	}
	if spent, _ := store.SpendToday(context.Background(), "g1", today(testNow)); spent.SpentUSD <= 0 {
		t.Error("the call was not booked against the budget")
	}
}

func TestCompleteSaysWhenItCannotRun(t *testing.T) {
	// No key at all.
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.APIKeySealed = nil
	chat := &scriptedChat{out: "x"}
	p := completePlugin(t, store, cfg, chat)
	if _, err := p.Complete(context.Background(), "g1", "s", "u"); !errors.Is(err, ErrNoKey) || chat.calls != 0 {
		t.Errorf("no key: err %v calls %d", err, chat.calls)
	}
	var u interface{ Unavailable() bool }
	if err := ErrNoKey; !errors.As(err, &u) || !u.Unavailable() {
		t.Error("ErrNoKey should report itself unavailable through the anonymous interface")
	}

	// Budget spent.
	store2 := newFakeStore()
	cfg2 := enforcingConfig()
	cfg2.DailyBudgetUSD = 0.001
	_ = store2.AddSpend(context.Background(), "g1", today(testNow), Usage{Cost: 5}, true)
	p2 := completePlugin(t, store2, cfg2, chat)
	if _, err := p2.Complete(context.Background(), "g1", "s", "u"); !errors.Is(err, ErrBudgetExhausted) || chat.calls != 0 {
		t.Errorf("budget: err %v calls %d", err, chat.calls)
	}

	// Disabled in the guild.
	store3 := newFakeStore()
	p3 := completePlugin(t, store3, enforcingConfig(), chat)
	p3.WithGate(gateOff{})
	if _, err := p3.Complete(context.Background(), "g1", "s", "u"); !errors.Is(err, ErrNoKey) || chat.calls != 0 {
		t.Errorf("disabled: err %v calls %d", err, chat.calls)
	}

	// A 402 from the gateway is the budget, however the row reads.
	store4 := newFakeStore()
	chat4 := &scriptedChat{err: &APIError{Status: 402, Message: "insufficient credits"}}
	p4 := completePlugin(t, store4, enforcingConfig(), chat4)
	if _, err := p4.Complete(context.Background(), "g1", "s", "u"); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("402: err %v", err)
	}
	// Any other failure is a failure, not unavailability.
	chat4.err = errors.New("gateway exploded")
	_, err := p4.Complete(context.Background(), "g1", "s", "u")
	if err == nil || errors.As(err, &u) {
		t.Errorf("a plain failure reported as unavailable: %v", err)
	}
}

type gateOff struct{}

func (gateOff) PluginEnabled(string, string) bool { return false }
