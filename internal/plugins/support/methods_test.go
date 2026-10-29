package support

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	cards "github.com/6586x57890143/cygnet/support"
)

func TestPaymentMethodsCacheAndOutageFallback(t *testing.T) {
	p, _ := testPlugin(t, fakeSettings{})
	now := time.Now()
	p.now = func() time.Time { return now }
	if got := p.paymentMethods(context.Background()); got == nil || len(got) != 0 {
		t.Fatal("unconfigured offer must be explicitly empty")
	}
	calls := 0
	failed := false
	p.WithPaymentMethods(func(ctx context.Context) ([]cards.Method, error) {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Second {
			t.Error("offer call has no response deadline")
		}
		if failed {
			return nil, errors.New("hub offline")
		}
		return []cards.Method{{Key: "base:ETH"}, {Key: "monero:XMR"}}, nil
	})
	got := p.paymentMethods(context.Background())
	got[0].Key = "modified"
	if p.paymentMethods(context.Background())[0].Key != "base:ETH" || calls != 1 {
		t.Fatal("cache leaked its slice or refetched")
	}
	now = now.Add(6 * time.Minute)
	failed = true
	if got := p.paymentMethods(context.Background()); got == nil || len(got) != 0 {
		t.Fatal("outage advertised stale rails")
	}
	p.paymentMethods(context.Background())
	if calls != 2 {
		t.Fatal("outage wasn't cached briefly")
	}
	now = now.Add(31 * time.Second)
	failed = false
	if len(p.paymentMethods(context.Background())) != 2 || calls != 3 {
		t.Fatal("offer didn't recover")
	}
}

func TestConcurrentAdvertsDoNotMultiplyOfferCalls(t *testing.T) {
	p, _ := testPlugin(t, fakeSettings{})
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p.WithPaymentMethods(func(context.Context) ([]cards.Method, error) {
		calls.Add(1)
		close(started)
		<-release
		return []cards.Method{{Key: "base:ETH"}}, nil
	})
	go func() { defer close(done); p.paymentMethods(context.Background()) }()
	<-started
	if got := p.paymentMethods(context.Background()); len(got) != 0 || calls.Load() != 1 {
		t.Fatal("parallel advert waited or fetched again")
	}
	close(release)
	<-done
	if len(p.paymentMethods(context.Background())) != 1 || calls.Load() != 1 {
		t.Fatal("completed offer not cached")
	}
}
