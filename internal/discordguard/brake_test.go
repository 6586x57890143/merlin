package discordguard

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

type answer struct {
	status int
	scope  string
	sent   int
}

func (a *answer) RoundTrip(*http.Request) (*http.Response, error) {
	a.sent++
	h := http.Header{}
	if a.scope != "" {
		h.Set("X-RateLimit-Scope", a.scope)
	}
	return &http.Response{StatusCode: a.status, Header: h, Body: http.NoBody}, nil
}

func TestBrakeStopsBeforeTheIPBan(t *testing.T) {
	at := time.Unix(1_000_000_000, 0)
	a := &answer{status: http.StatusForbidden}
	trips := 0
	b := &brake{next: a, budget: 5, now: func() time.Time { return at }, tripped: func(int) { trips++ }}
	req, _ := http.NewRequest(http.MethodDelete, "https://discord.com/api/v10/x", nil)
	for range 5 {
		if _, err := b.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := b.RoundTrip(req); !errors.Is(err, ErrBraked) {
			t.Fatalf("past the budget: %v", err)
		}
	}
	if a.sent != 5 || trips != 1 {
		t.Fatalf("sent %d, tripped %d; want 5 and 1", a.sent, trips)
	}
	at = at.Add(brakeWindow)
	if _, err := b.RoundTrip(req); err != nil {
		t.Fatalf("a window later: %v", err)
	}
}

func TestBrakeCountsOnlyWhatDiscordCounts(t *testing.T) {
	now := func() time.Time { return time.Unix(1_000_000_000, 0) }
	for _, a := range []*answer{{status: 200}, {status: 404}, {status: 429, scope: "shared"}} {
		b := &brake{next: a, budget: 1, now: now}
		req, _ := http.NewRequest(http.MethodGet, "https://discord.com/api/v10/x", nil)
		for range 3 {
			if _, err := b.RoundTrip(req); err != nil {
				t.Fatalf("%d %s braked: %v", a.status, a.scope, err)
			}
		}
	}
	for _, a := range []*answer{{status: 401}, {status: 429}, {status: 429, scope: "user"}} {
		b := &brake{next: a, budget: 1, now: now}
		req, _ := http.NewRequest(http.MethodGet, "https://discord.com/api/v10/x", nil)
		_, _ = b.RoundTrip(req)
		if _, err := b.RoundTrip(req); !errors.Is(err, ErrBraked) {
			t.Fatalf("%d %s not counted", a.status, a.scope)
		}
	}
}
