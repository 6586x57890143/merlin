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
	if !strings.Contains(privacyLine(openRouter, Config{}), "zero data retention") {
		t.Errorf("OpenRouter line does not mention what it pins: %q", privacyLine(openRouter, Config{}))
	}
	orca := privacyLine(orcaRouter, Config{})
	if !strings.Contains(orca, "No per-request retention control") {
		t.Errorf("OrcaRouter line does not say the guarantee is absent: %q", orca)
	}
	if strings.EqualFold(orca, privacyLine(openRouter, Config{})) {
		t.Error("both gateways claim the same guarantee")
	}
}

// A free-only stack under the strict block is unroutable (404 "No endpoints
// found matching your data policy"), so it goes without retention terms and
// the status screen says so. A mixed stack keeps them.
func TestFreeOnlyStackDropsRetentionTerms(t *testing.T) {
	free := []string{"minimax/minimax-m3.1-flash:free"}
	req := relaxForFree(chatRequest{Models: free, Provider: strictProvider(), ResponseFormat: fastSchema(),
		Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if p := req.Provider; p.ZDR || p.DataCollection != "allow" || p.RequireParameters {
		t.Errorf("free stack sent %+v, want no ZDR, collection allowed, parameters not required", *p)
	}
	// The second 404: free endpoints take response_format but not
	// structured_outputs, so the schema moves into the prompt.
	if req.ResponseFormat.Type != "json_object" || len(req.Messages) != 2 ||
		!strings.Contains(req.Messages[0].Content, `"v"`) {
		t.Errorf("schema not carried into the prompt: %+v / %+v", req.ResponseFormat, req.Messages)
	}
	raw, _ := json.Marshal(req.ResponseFormat)
	if string(raw) != `{"type":"json_object"}` {
		t.Errorf("json_object request still carries a schema: %s", raw)
	}

	if got := relaxForFree(chatRequest{Models: []string{"openrouter/free"}, Provider: strictProvider()}); got.Provider.ZDR {
		t.Error("the free router kept the strict block, which cannot route")
	}

	mixed := append(free, "openai/gpt-5-nano")
	if got := relaxForFree(chatRequest{Models: mixed, Provider: strictProvider()}); !got.Provider.ZDR || !got.Provider.RequireParameters {
		t.Errorf("mixed stack lost its terms: %+v", *got.Provider)
	}
	if relaxForFree(chatRequest{Models: free}).Provider != nil {
		t.Error("a gateway that takes no block was handed one")
	}
	if !relaxForFree(chatRequest{Provider: strictProvider()}).Provider.ZDR {
		t.Error("an unpinned stack (gateway defaults) lost its retention terms")
	}
	if line := privacyLine(openRouter, Config{FastModels: free}); !strings.Contains(line, "Free models only") {
		t.Errorf("status does not disclose the free-stack trade: %q", line)
	}
	for in, want := range map[string]string{"```json\n{\"v\":[]}\n```": `{"v":[]}`, ` {"v":[]} `: `{"v":[]}`} {
		if got := unfence(in); got != want {
			t.Errorf("unfence(%q) = %q, want %q", in, got, want)
		}
	}
}

// ZDR is the preferred default, so a stack that gives it up says so when set.
func TestSettingAFreeStackWarns(t *testing.T) {
	if zdrWarning([]string{"openai/gpt-5-nano"}) != "" {
		t.Error("a paid stack keeps ZDR and should not warn")
	}
	if !strings.Contains(zdrWarning([]string{"x/y:free"}), "may log or train") {
		t.Error("a free-only stack gives up ZDR without saying so")
	}
	if !strings.Contains(zdrWarning([]string{"x/y:free", "openai/gpt-5-nano"}), "skipped") {
		t.Error("a mixed stack does not say its free entries will never answer")
	}
}
