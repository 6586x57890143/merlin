package aimod

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// The budget is somebody else's money. Every failure mode here has to point
// the same way: spend less than asked rather than more, and never turn an
// unreadable counter into an uncapped one.

func TestTodayIsAUTCDateKey(t *testing.T) {
	// Local midnight is not the boundary; OpenRouter's own daily counters
	// roll at UTC, and two systems disagreeing about when the day was is a
	// worse problem than an awkward hour.
	late := time.Date(2026, 3, 14, 23, 59, 59, 0, time.FixedZone("east", 10*60*60))
	if got := today(late); got.Location() != time.UTC {
		t.Errorf("day key is in %v, want UTC", got.Location())
	}
	if got := today(late); got.Hour() != 0 || got.Minute() != 0 {
		t.Errorf("day key = %v, want midnight", got)
	}
	sameDay := today(testNow).Equal(today(testNow.Add(11 * time.Hour)))
	if !sameDay {
		t.Error("two times on the same UTC day produced different keys")
	}
	if today(testNow).Equal(today(testNow.Add(24 * time.Hour))) {
		t.Error("a day later produced the same key, so the budget never resets")
	}
}

func TestBudgetBlocksOnceSpent(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	sealer, err := newSealer(testSecretKey)
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	p.sealer = sealer
	sealed, err := sealer.seal("sk-or-v1-test")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	cfg := Config{GuildID: "g1", APIKeySealed: sealed, DailyBudgetUSD: 0.10}

	state, err := p.checkBudget(context.Background(), cfg)
	if err != nil {
		t.Fatalf("checkBudget: %v", err)
	}
	if state.Exhausted {
		t.Fatal("exhausted before anything was spent")
	}
	if state.APIKey != "sk-or-v1-test" {
		t.Errorf("APIKey = %q, want the unsealed key", state.APIKey)
	}

	// Spend right up to the cap.
	p.recordUsage(context.Background(), "g1", Usage{Cost: 0.09, PromptTokens: 100}, false)
	if state, _ := p.checkBudget(context.Background(), cfg); state.Exhausted {
		t.Error("exhausted at $0.09 of a $0.10 budget")
	}
	p.recordUsage(context.Background(), "g1", Usage{Cost: 0.02, PromptTokens: 100}, false)
	state, _ = p.checkBudget(context.Background(), cfg)
	if !state.Exhausted {
		t.Errorf("spent %.2f of %.2f and was not exhausted", state.Spent, state.Budget)
	}

	// Tomorrow is a new day, with no reset job involved.
	p.now = func() time.Time { return testNow.Add(24 * time.Hour) }
	if state, _ := p.checkBudget(context.Background(), cfg); state.Exhausted {
		t.Error("yesterday's spend is still counted against today's budget")
	}
}

// An unreadable spend row must not become an uncapped budget. Assuming zero
// spent is the single worst direction to be wrong about somebody's money.
func TestUnreadableSpendFailsClosed(t *testing.T) {
	store := newFakeStore()
	store.spendErr = errors.New("database is down")
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	sealer, _ := newSealer(testSecretKey)
	p.sealer = sealer
	sealed, _ := sealer.seal("k")

	state, err := p.checkBudget(context.Background(), Config{GuildID: "g1", APIKeySealed: sealed, DailyBudgetUSD: 10})
	if err == nil {
		t.Error("an unreadable spend row was reported as fine")
	}
	if !state.Exhausted {
		t.Error("an unreadable spend row left the budget open")
	}
}

func TestNoKeyMeansNoSpending(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	state, err := p.checkBudget(context.Background(), Config{GuildID: "g1", DailyBudgetUSD: 10})
	if err != nil {
		t.Fatalf("checkBudget: %v", err)
	}
	if !state.Exhausted {
		t.Error("a guild with no API key was cleared to spend")
	}
}

// A budget that only counts calls whose body parsed is a budget a
// misbehaving model can walk straight through, so usage is booked whenever
// the response carried any.
func TestUsageIsBookedEvenWhenTheAnswerIsUnusable(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{
		fast:  []string{"not json at all"},
		usage: Usage{Cost: 0.004, PromptTokens: 900, CompletionTokens: 8},
	}
	p := testPlugin(t, store, client, newFakeOps(), &fakeAudit{})
	sealer, _ := newSealer(testSecretKey)
	p.sealer = sealer
	sealed, _ := sealer.seal("k")

	cfg := enforcingConfig()
	cfg.APIKeySealed = sealed
	store.setConfig(cfg)

	p.classify("g1", []candidate{{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "hello"}})
	p.wg.Wait()

	spend, err := store.SpendToday(context.Background(), "g1", today(testNow))
	if err != nil {
		t.Fatalf("SpendToday: %v", err)
	}
	if spend.SpentUSD == 0 {
		t.Error("a billed call whose body failed to parse was not counted against the budget")
	}
	if spend.Scanned == 0 {
		t.Error("the scanned count did not move, so every cost estimate divides by zero")
	}
}

// The audit entry for an exhausted budget fires once a day, not once a
// message. The difference is a useful warning versus a flooded channel.
func TestBudgetNoticeIsOncePerDay(t *testing.T) {
	audit := &fakeAudit{}
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), audit)
	state := budgetState{Spent: 1, Budget: 1, Exhausted: true}

	for range 5 {
		p.noticeBudget(context.Background(), "g1", state)
	}
	if n := len(audit.actions()); n != 1 {
		t.Errorf("posted %d budget notices in one day, want 1", n)
	}

	p.now = func() time.Time { return testNow.Add(24 * time.Hour) }
	p.noticeBudget(context.Background(), "g1", state)
	if n := len(audit.actions()); n != 2 {
		t.Errorf("posted %d notices across two days, want 2", n)
	}
}

func TestEstimateSaysWhenItIsGuessing(t *testing.T) {
	fast := Model{ID: "cheap/model", PromptPerM: 0.05, CompletionPerM: 0.40}
	deep := Model{ID: "good/model", PromptPerM: 0.25, CompletionPerM: 2.00}

	// No history at all: the numbers are assumptions and must say so, since
	// an admin is about to set a budget from them.
	blind := estimateFor(nil, fast, deep)
	if blind.Measured {
		t.Error("an estimate with no history claimed to be measured")
	}
	if blind.USDPerDay <= 0 {
		t.Error("the assumed estimate came out at zero, which tells an admin the feature is free")
	}

	measured := estimateFor([]Spend{{
		Day: testNow, Scanned: 1000, FastCalls: 50, DeepCalls: 10,
		FastPromptTokens: 60000, FastCompletionTokens: 500,
		DeepPromptTokens: 16000, DeepCompletionTokens: 400,
	}}, fast, deep)
	if !measured.Measured {
		t.Error("an estimate built on a real day of traffic did not report itself as measured")
	}
	if measured.ScannedPerDay != 1000 {
		t.Errorf("ScannedPerDay = %v, want 1000", measured.ScannedPerDay)
	}
	if measured.DeepRate != 0.01 {
		t.Errorf("DeepRate = %v, want 0.01", measured.DeepRate)
	}
}

// A more expensive model must project a higher bill. Obvious, and exactly
// the sort of thing a sign flip in the blend would invert silently.
func TestEstimateRisesWithPrice(t *testing.T) {
	history := []Spend{{
		Day: testNow, Scanned: 1000, FastCalls: 50, DeepCalls: 10,
		FastPromptTokens: 60000, DeepPromptTokens: 16000,
	}}
	cheap := estimateFor(history, Model{PromptPerM: 0.05}, Model{PromptPerM: 0.25})
	dear := estimateFor(history, Model{PromptPerM: 5.00}, Model{PromptPerM: 0.25})

	if dear.USDPerDay <= cheap.USDPerDay {
		t.Errorf("a 100x more expensive fast model projected %v against %v", dear.USDPerDay, cheap.USDPerDay)
	}
}

// A nano model on a quiet server costs fractions of a cent a day, and
// printing that as "$0.00" tells an admin the feature is free.
func TestFormatUSDStaysHonestAtBothEnds(t *testing.T) {
	tests := map[float64]string{
		0:       "$0.00",
		0.00004: "$0.00004",
		0.0123:  "$0.0123",
		1.5:     "$1.50",
		42:      "$42.00",
	}
	for in, want := range tests {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPerMillionConvertsAndSurvivesJunk(t *testing.T) {
	// Compared with a tolerance: the conversion is a float multiply and the
	// value is only ever displayed, so exact equality would be asserting
	// something this function does not promise.
	if got := perMillion("0.00000005"); math.Abs(got-0.05) > 1e-9 {
		t.Errorf("perMillion = %v, want 0.05 per million tokens", got)
	}
	if got := perMillion(""); got != 0 {
		t.Errorf("an unparseable price gave %v, want 0", got)
	}
}
