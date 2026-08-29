package aimod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gateway a guild is on is derived from which keys it holds, never
// stored. These are the consequences of that rule.

func TestRouteDefaultsToOrcaRouterWhenItHasAKey(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want *providerSpec
	}{
		{"orca only", Config{OrcaKeySealed: []byte("o")}, orcaRouter},
		{"both", Config{OrcaKeySealed: []byte("o"), APIKeySealed: []byte("r")}, orcaRouter},
		{"openrouter only", Config{APIKeySealed: []byte("r")}, openRouter},
		// No keys at all still names a gateway rather than nil: every caller
		// reads spec.fastModels off it before checking whether there is a
		// credential, and a nil here would be a panic on a guild that has
		// simply not been configured yet.
		{"neither", Config{}, openRouter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, sealed := route(tc.cfg)
			if spec != tc.want {
				t.Fatalf("route = %s, want %s", spec.name, tc.want.name)
			}
			if want := sealedKeyFor(tc.cfg, spec); string(sealed) != string(want) {
				t.Fatalf("sealed = %q, want %q", sealed, want)
			}
		})
	}
}

// The prefix sniff is what keeps this to one option in the common case. It
// must not claim a key it does not recognise, since the handler turns nil
// into "say which" rather than into a refusal.
func TestProviderForKey(t *testing.T) {
	cases := map[string]*providerSpec{
		"sk-orca-abc123":      orcaRouter,
		"sk-or-v1-abc123":     openRouter,
		"sk-proj-somethingel": nil,
		"":                    nil,
		"sk-orca-":            nil,
	}
	for key, want := range cases {
		if got := providerForKey(key); got != want {
			t.Errorf("providerForKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// OrcaRouter is OpenAI-shaped: no models array, no provider object, and no
// cost in the usage block unless it is asked for. Asserted against the wire
// rather than the struct, because that is what the gateway actually sees.
func TestOrcaRouterRequestShape(t *testing.T) {
	var body map[string]any
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)

	spec := *orcaRouter
	spec.base = srv.URL
	c := NewClient()
	if _, _, err := c.Chat(context.Background(), "sk-orca-k", chatRequest{
		spec:     &spec,
		Models:   []string{"orcarouter/free", "never/sent"},
		Provider: strictProvider(),
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// A request carrying neither model nor models is rejected outright, so
	// this is the difference between scanning and not scanning.
	if body["model"] != "orcarouter/free" {
		t.Errorf("model = %v, want the first entry of the stack", body["model"])
	}
	if _, ok := body["models"]; ok {
		t.Error("sent the fallback array to a gateway that has no array form")
	}
	// The privacy block is stripped rather than sent and ignored. A block
	// this gateway silently drops would leave the code around it reading as
	// though the guarantee still held.
	if _, ok := body["provider"]; ok {
		t.Error("sent provider preferences to a gateway that does not accept them")
	}
	if got := header.Get(orcaCostHeader); got != "true" {
		t.Errorf("%s = %q, want true: without it every call reads as free and the budget stops being one",
			orcaCostHeader, got)
	}
}

// The other half of that: the cost comes back under a different name, and
// everything downstream of here reads one field.
func TestCostUSDIsFoldedIntoCost(t *testing.T) {
	c, _ := stubOpenRouter(t, http.StatusOK, `{
		"choices": [{"message": {"content": "ok"}}],
		"usage": {"prompt_tokens": 30, "total_tokens": 40, "cost_usd": 0.0021}
	}`)
	_, usage, err := c.Chat(context.Background(), "k", chatRequest{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if usage.Cost != 0.0021 {
		t.Fatalf("Cost = %v, want the cost_usd figure: a gateway reporting under the other name would bill silently", usage.Cost)
	}
}

// A free-tier window refills at its boundary rather than easing back, so the
// ordinary answer to a 429 (wait a moment, try the same gateway again) is the
// one thing that is certainly wrong. The oversized-prompt refusal reports
// identically and never passes on retry at all.
func TestFreeRateLimitIsNotRetriedOnTheSameGateway(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"free_rate_limited: daily free quota reached"}}`))
	}))
	t.Cleanup(srv.Close)

	spec := *orcaRouter
	spec.base = srv.URL
	c := NewClient()
	_, _, err := c.Chat(context.Background(), "sk-orca-k", chatRequest{spec: &spec})
	if err == nil {
		t.Fatal("Chat succeeded against a gateway returning 429")
	}
	if calls != 1 {
		t.Fatalf("made %d calls, want 1: a free window does not refill during a short pause", calls)
	}
	if !freeRateLimited(err) {
		t.Errorf("freeRateLimited(%v) = false, so the deep rung would not know to fall over", err)
	}
	// An ordinary 429 keeps the old behaviour, or this guard would have
	// quietly removed the retry the other gateway still needs.
	if freeRateLimited(&APIError{Status: http.StatusTooManyRequests, Message: "Rate limit exceeded"}) {
		t.Error("an ordinary rate limit is being treated as a free-tier one")
	}
}

// The privacy difference is stated on the status screen, not buried in the
// docs: it is the one promise this plugin makes about member text, and the
// default gateway is the weaker of the two on it.
func TestPrivacyLineNamesTheWeakerGuarantee(t *testing.T) {
	if !strings.Contains(privacyLine(openRouter), "zero data retention") {
		t.Errorf("OpenRouter line does not mention what it pins: %q", privacyLine(openRouter))
	}
	orca := privacyLine(orcaRouter)
	if !strings.Contains(orca, "No per-request retention control") {
		t.Errorf("OrcaRouter line does not say the guarantee is absent: %q", orca)
	}
	if strings.EqualFold(orca, privacyLine(openRouter)) {
		t.Error("both gateways claim the same guarantee")
	}
}
