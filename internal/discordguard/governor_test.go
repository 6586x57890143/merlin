package discordguard

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
)

// testClock lets the governor's time-based behavior be exercised without
// sleeping, mirroring the fake clock the Scheduler's own tests use.
type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newGovernedGuard(gate GuildGate) (*Guard, *wire, *testClock) {
	clock := &testClock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	g, w := restGuard(gate, `{}`)
	g.now = clock.Now
	return g, w, clock
}

// The cap exists so a bug or a malicious actor can't spam channel deletions
// until Discord rate-limits or bans the whole application (spec.MD §4).
func TestRateCapRefusesPastTheHourlyLimit(t *testing.T) {
	g, w, _ := newGovernedGuard(fakeGate{})
	o := g.For("100")

	limit := opCaps[opChannelDelete]
	for i := range limit {
		if err := o.DeleteChannel(2); err != nil {
			t.Fatalf("call %d of %d refused early: %v", i+1, limit, err)
		}
	}
	if err := o.DeleteChannel(2); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call %d returned %v, want ErrRateLimited", limit+1, err)
	}
	if len(w.requests) != limit {
		t.Errorf("%d calls reached Discord, want %d", len(w.requests), limit)
	}
}

// Refusals must not be mistaken for the deliberate skips a paused guild
// produces: a guild burning through its cap is abnormal and needs to reach
// the Scheduler's failure alerting, not be silently swallowed.
func TestRateAndCircuitErrorsAreNotSkips(t *testing.T) {
	if Skipped(ErrRateLimited) {
		t.Error("ErrRateLimited must not count as a deliberate skip")
	}
	if Skipped(ErrCircuitOpen) {
		t.Error("ErrCircuitOpen must not count as a deliberate skip")
	}
}

// Budget refills over time rather than resetting on a clock boundary, so a
// guild can't bank a burst by staying quiet.
func TestRateCapRefillsOverTime(t *testing.T) {
	g, _, clock := newGovernedGuard(fakeGate{})
	o := g.For("100")

	limit := opCaps[opChannelDelete]
	for range limit {
		if err := o.DeleteChannel(2); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	}
	if err := o.DeleteChannel(2); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want ErrRateLimited", err)
	}

	// A tenth of the window back is a tenth of the capacity back.
	clock.advance(capWindow / 10)
	if err := o.DeleteChannel(2); err != nil {
		t.Errorf("after refill: %v", err)
	}
}

// Caps are per guild: one server exhausting its budget must not stop another.
func TestRateCapIsPerGuild(t *testing.T) {
	g, _, _ := newGovernedGuard(fakeGate{})

	limit := opCaps[opChannelDelete]
	for range limit {
		if err := g.For("100").DeleteChannel(2); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	}
	if err := g.For("100").DeleteChannel(2); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("busy guild: got %v, want ErrRateLimited", err)
	}
	if err := g.For("200").DeleteChannel(2); err != nil {
		t.Errorf("quiet guild was refused: %v", err)
	}
}

func TestCircuitBreakerOpensOnSustained5xxAndRecovers(t *testing.T) {
	g, w, clock := newGovernedGuard(fakeGate{})
	w.status = http.StatusInternalServerError
	o := g.For("100")

	for i := range breakerThreshold {
		if err := o.PinMessage(2, 4); errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("breaker opened early, on call %d", i+1)
		}
	}

	before := len(w.requests)
	if err := o.PinMessage(2, 4); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v, want ErrCircuitOpen", err)
	}
	if len(w.requests) != before {
		t.Error("an open breaker still let a call through to Discord")
	}

	// Still open partway through the cooldown.
	clock.advance(breakerCooldown / 2)
	if err := o.PinMessage(2, 4); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("breaker reopened too early: %v", err)
	}

	// After the cooldown one probe gets through; Discord is healthy again,
	// so the breaker closes and normal service resumes.
	clock.advance(breakerCooldown)
	w.status = 0
	if err := o.PinMessage(2, 4); err != nil {
		t.Fatalf("probe after cooldown was refused: %v", err)
	}
	if err := o.PinMessage(2, 4); err != nil {
		t.Errorf("breaker did not close after a successful probe: %v", err)
	}
}

// A 4xx means this request was wrong: a deleted channel, a role above the
// bot. Letting those open the breaker would stop every unrelated write in the
// guild because one misconfigured rotation kept failing.
func TestCircuitBreakerIgnoresClientErrors(t *testing.T) {
	g, w, _ := newGovernedGuard(fakeGate{})
	w.status, w.body = http.StatusForbidden, `{"code":50013,"message":"Missing Permissions"}`
	o := g.For("100")

	for range breakerThreshold * 2 {
		if err := o.PinMessage(2, 4); errors.Is(err, ErrCircuitOpen) {
			t.Fatal("client errors opened the circuit breaker")
		}
	}
}

// A paused guild is checked before the governor, so it neither spends budget
// nor trips the breaker on calls it never made, or unpausing could
// land straight in a rate limit earned entirely by refusals.
func TestPauseDoesNotConsumeRateBudget(t *testing.T) {
	gate := fakeGate{paused: map[string]bool{"100": true}}
	g, _, _ := newGovernedGuard(gate)

	limit := opCaps[opChannelDelete]
	for range limit * 3 {
		if err := g.For("100").DeleteChannel(2); !errors.Is(err, ErrPaused) {
			t.Fatalf("got %v, want ErrPaused", err)
		}
	}

	// Unpause: the full budget must still be there.
	gate.paused["100"] = false
	for i := range limit {
		if err := g.For("100").DeleteChannel(2); err != nil {
			t.Fatalf("call %d after unpausing was refused: %v; the pause spent real budget", i+1, err)
		}
	}
}

func restErr(status, code int) error {
	return &rest.Error{Response: &http.Response{StatusCode: status}, Code: rest.JSONErrorCode(code)}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"500", restErr(http.StatusInternalServerError, 0), true},
		{"503", restErr(http.StatusServiceUnavailable, 0), true},
		{"429", restErr(http.StatusTooManyRequests, 0), true},
		{"403 missing permissions", restErr(http.StatusForbidden, 50013), false},
		{"404 unknown channel", restErr(http.StatusNotFound, 10003), false},
		{"no response", &rest.Error{}, false},
		{"bare network error", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := isTransient(tc.err); got != tc.want {
			t.Errorf("%s: isTransient = %v, want %v", tc.name, got, tc.want)
		}
	}
}
