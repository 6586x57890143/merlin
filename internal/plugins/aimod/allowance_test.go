package aimod

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// gatedClassifier holds every call until released, charging cost for each,
// so a test can put several calls in flight at once.
type gatedClassifier struct {
	cost    float64
	gate    chan struct{}
	mu      sync.Mutex
	in, max int
}

func (g *gatedClassifier) Chat(ctx context.Context, _ string, _ chatRequest) (string, Usage, error) {
	g.mu.Lock()
	g.in++
	g.max = max(g.max, g.in)
	g.mu.Unlock()
	<-g.gate
	g.mu.Lock()
	g.in--
	g.mu.Unlock()
	return "{}", Usage{Cost: g.cost, TotalTokens: 1}, nil
}

// The cap used to be checked once per batch, so overlapping batches and
// their escalations spent past it together. Every call now reserves against
// it first, so a pile of concurrent calls on a nearly spent budget cannot
// go over, and no more than maxPaidCalls are ever on the wire at once.
func TestOverlappingCallsCannotSpendPastTheCap(t *testing.T) {
	store := newFakeStore()
	gc := &gatedClassifier{cost: 0.02, gate: make(chan struct{})}
	p := testPlugin(t, store, gc, newFakeOps(), &fakeAudit{})
	state := budgetState{APIKey: "key", Spec: openRouter, Budget: 0.10}

	const calls = 12
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := p.paidChat(context.Background(), "g1", state, true, chatRequest{})
			errs <- err
		}()
	}
	close(gc.gate)
	wg.Wait()
	close(errs)

	refused := 0
	for err := range errs {
		if errors.Is(err, errOverBudget) {
			refused++
		} else if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	spent, _ := store.SpendToday(context.Background(), "g1", today(p.now()))
	if spent.SpentUSD > state.Budget+1e-9 {
		t.Errorf("spent $%.2f of a $%.2f cap", spent.SpentUSD, state.Budget)
	}
	if refused == 0 {
		t.Error("twelve $0.02 calls on a $0.10 cap were all admitted")
	}
	if gc.max > maxPaidCalls {
		t.Errorf("%d calls were in flight at once, want at most %d", gc.max, maxPaidCalls)
	}
}

// A receipt that could not be written leaves today's total understated, so
// nothing more is spent until it lands.
func TestALostReceiptStopsPaidCalls(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{usage: Usage{Cost: 0.01, TotalTokens: 10}}
	p := testPlugin(t, store, client, newFakeOps(), &fakeAudit{})
	state := budgetState{APIKey: "key", Spec: openRouter, Budget: 5}

	store.addSpendErr = errors.New("database unreachable")
	if _, _, err := p.paidChat(context.Background(), "g1", state, false, chatRequest{}); err != nil {
		t.Fatalf("the call itself failed: %v", err)
	}
	if _, _, err := p.paidChat(context.Background(), "g1", state, false, chatRequest{}); !errors.Is(err, errSpendUnbooked) {
		t.Fatalf("a call was made while the last one's cost was unrecorded: %v", err)
	}

	store.addSpendErr = nil
	if _, _, err := p.paidChat(context.Background(), "g1", state, false, chatRequest{}); err != nil {
		t.Fatalf("paid calls did not resume once the receipt could be written: %v", err)
	}
	spent, _ := store.SpendToday(context.Background(), "g1", today(p.now()))
	if spent.FastCalls != 2 {
		t.Errorf("booked %d calls, want 2: the held receipt and the new one", spent.FastCalls)
	}
}

// A guild with a few cents a day must still be able to make its first
// call, which is what teaches it a real per-call figure.
func TestASmallBudgetStillAdmitsACall(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	state := budgetState{APIKey: "key", Spec: openRouter, Budget: 0.05}
	if _, _, err := p.paidChat(context.Background(), "g1", state, true, chatRequest{}); err != nil {
		t.Fatalf("a $0.05 budget refused its first deep call: %v", err)
	}
	state.Budget = 0
	if _, _, err := p.paidChat(context.Background(), "g1", state, true, chatRequest{}); !errors.Is(err, errOverBudget) {
		t.Fatalf("a zero budget admitted a call: %v", err)
	}
}
