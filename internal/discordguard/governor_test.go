package discordguard

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// testClock lets the governor's time-based behavior be exercised without
// sleeping — mirrors the fake clock the Scheduler's own tests use.
type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newGovernedGuard(sess Session) (*Guard, *testClock) {
	clock := &testClock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	g := New(sess, fakeGate{}, testLogger(), false)
	g.now = clock.Now
	return g, clock
}

func restErr(status, code int) error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: status},
		Message:  &discordgo.APIErrorMessage{Code: code},
	}
}

// The cap exists so a bug or a malicious actor can't spam channel deletions
// until Discord rate-limits or bans the whole application (spec.MD §4).
func TestRateCapRefusesPastTheHourlyLimit(t *testing.T) {
	sess := &fakeSession{}
	g, _ := newGovernedGuard(sess)
	o := g.For("g1")

	limit := opCaps[opChannelDelete]
	for i := range limit {
		if _, err := o.ChannelDelete("c"); err != nil {
			t.Fatalf("call %d of %d refused early: %v", i+1, limit, err)
		}
	}
	if _, err := o.ChannelDelete("c"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call %d returned %v, want ErrRateLimited", limit+1, err)
	}
	if sess.writes != limit {
		t.Errorf("%d calls reached Discord, want %d", sess.writes, limit)
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
	g, clock := newGovernedGuard(&fakeSession{})
	o := g.For("g1")

	limit := opCaps[opRoleCreate]
	for range limit {
		if _, err := o.GuildRoleCreate("g1", &discordgo.RoleParams{}); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	}
	if _, err := o.GuildRoleCreate("g1", &discordgo.RoleParams{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want ErrRateLimited", err)
	}

	// A tenth of the window back is a tenth of the capacity back.
	clock.advance(capWindow / 10)
	if _, err := o.GuildRoleCreate("g1", &discordgo.RoleParams{}); err != nil {
		t.Errorf("after refill: %v", err)
	}
}

// Caps are per guild: one server exhausting its budget must not stop another.
func TestRateCapIsPerGuild(t *testing.T) {
	g, _ := newGovernedGuard(&fakeSession{})

	limit := opCaps[opChannelDelete]
	for range limit {
		if _, err := g.For("busy").ChannelDelete("c"); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	}
	if _, err := g.For("busy").ChannelDelete("c"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("busy guild: got %v, want ErrRateLimited", err)
	}
	if _, err := g.For("quiet").ChannelDelete("c"); err != nil {
		t.Errorf("quiet guild was refused: %v", err)
	}
}

// failingSession returns a fixed error from every write, so the breaker can
// be driven without a real Discord outage.
type failingSession struct {
	fakeSession
	err error
}

func (f *failingSession) ChannelEditComplex(string, *discordgo.ChannelEdit, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.writes++
	return nil, f.err
}

func TestCircuitBreakerOpensOnSustained5xxAndRecovers(t *testing.T) {
	sess := &failingSession{err: restErr(http.StatusInternalServerError, 0)}
	g, clock := newGovernedGuard(sess)
	o := g.For("g1")

	for i := range breakerThreshold {
		if _, err := o.ChannelEditComplex("c", &discordgo.ChannelEdit{}); errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("breaker opened early, on call %d", i+1)
		}
	}

	before := sess.writes
	if _, err := o.ChannelEditComplex("c", &discordgo.ChannelEdit{}); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v, want ErrCircuitOpen", err)
	}
	if sess.writes != before {
		t.Error("an open breaker still let a call through to Discord")
	}

	// Still open partway through the cooldown.
	clock.advance(breakerCooldown / 2)
	if _, err := o.ChannelEditComplex("c", &discordgo.ChannelEdit{}); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("breaker reopened too early: %v", err)
	}

	// After the cooldown one probe gets through; Discord is healthy again,
	// so the breaker closes and normal service resumes.
	clock.advance(breakerCooldown)
	sess.err = nil
	if _, err := o.ChannelEditComplex("c", &discordgo.ChannelEdit{}); err != nil {
		t.Fatalf("probe after cooldown was refused: %v", err)
	}
	if _, err := o.ChannelEditComplex("c", &discordgo.ChannelEdit{}); err != nil {
		t.Errorf("breaker did not close after a successful probe: %v", err)
	}
}

// A 4xx means this request was wrong — a deleted channel, a role above the
// bot. Letting those open the breaker would stop every unrelated write in the
// guild because one misconfigured rotation kept failing.
func TestCircuitBreakerIgnoresClientErrors(t *testing.T) {
	sess := &failingSession{err: restErr(http.StatusForbidden, 50013)}
	g, _ := newGovernedGuard(sess)
	o := g.For("g1")

	for range breakerThreshold * 2 {
		if _, err := o.ChannelEditComplex("c", &discordgo.ChannelEdit{}); errors.Is(err, ErrCircuitOpen) {
			t.Fatal("client errors opened the circuit breaker")
		}
	}
}

// A paused guild is checked before the governor, so it neither spends budget
// nor trips the breaker on calls it never made — otherwise unpausing could
// land straight in a rate limit earned entirely by refusals.
func TestPauseDoesNotConsumeRateBudget(t *testing.T) {
	sess := &fakeSession{}
	gate := fakeGate{paused: map[string]bool{"g1": true}}
	clock := &testClock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	g := New(sess, gate, testLogger(), false)
	g.now = clock.Now

	limit := opCaps[opChannelDelete]
	for range limit * 3 {
		if _, err := g.For("g1").ChannelDelete("c"); !errors.Is(err, ErrPaused) {
			t.Fatalf("got %v, want ErrPaused", err)
		}
	}

	// Unpause: the full budget must still be there.
	gate.paused["g1"] = false
	for i := range limit {
		if _, err := g.For("g1").ChannelDelete("c"); err != nil {
			t.Fatalf("call %d after unpausing was refused: %v — the pause spent real budget", i+1, err)
		}
	}
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
		{"bare network error", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := isTransient(tc.err); got != tc.want {
			t.Errorf("%s: isTransient = %v, want %v", tc.name, got, tc.want)
		}
	}
}
