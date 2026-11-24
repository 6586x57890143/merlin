package aimod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

func TestNamedModelsRunOnOpenRouter(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	chat := &scriptedChat{out: "{}", usage: Usage{Cost: 0.25, TotalTokens: 1}}
	p := completePlugin(t, store, enforcingConfig(), chat)
	cfg, _ := store.Config(ctx, "g1")
	cfg.OrcaKeySealed, cfg.DailyBudgetUSD = cfg.APIKeySealed, 5
	store.setConfig(cfg)
	q := core.Prompt{System: "s", User: "u", Models: []string{"a/b"}, Brief: true}
	if a, err := p.Ask(ctx, "g1", q); err != nil || a.Cost != 0.25 {
		t.Fatalf("Ask = %+v, %v", a, err)
	}
	if r := chat.last; r.spec != openRouter || !slices.Equal(r.Models, q.Models) || r.Provider == nil || !r.Provider.ZDR || r.effort != "low" {
		t.Errorf("request %+v", r)
	}
	if !p.HasOpenRouter(ctx, "g1") {
		t.Error("HasOpenRouter")
	}
	cfg.APIKeySealed = nil
	store.setConfig(cfg)
	if _, err := p.Ask(ctx, "g1", q); !errors.Is(err, ErrNoKey) || p.HasOpenRouter(ctx, "g1") {
		t.Errorf("no OpenRouter key: %v", err)
	}
	// A narrative is left to think.
	if _, err := p.Complete(ctx, "g1", "s", "u"); err != nil || chat.last.effort != "" {
		t.Errorf("effort %q, %v", chat.last.effort, err)
	}
}

// A model that won't stop thinking is asked to think little, where the call
// asks for that, and left alone where it doesn't.
func TestBriefThinking(t *testing.T) {
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		reasoning, _ := body["reasoning"].(map[string]any)
		sent = append(sent, reasoning)
		if on, ok := reasoning["enabled"]; ok && on == false {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := NewClient()
	c.base = srv.URL
	for _, effort := range []string{"low", "low", ""} {
		if _, _, err := c.Chat(context.Background(), "k", chatRequest{Models: []string{"a/b"}, effort: effort}); err != nil {
			t.Fatal(err)
		}
	}
	if len(sent) != 4 || sent[1]["effort"] != "low" || sent[2]["effort"] != "low" || sent[3] != nil {
		t.Errorf("sent %v", sent)
	}
	if _, ok := sent[1]["enabled"]; ok {
		t.Errorf("both at once: %v", sent[1])
	}
}

func TestPricesAreOfWhatCanBeRouted(t *testing.T) {
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads++; r.URL.Path != "/endpoints/zdr" {
			t.Errorf("read %s", r.URL.Path)
		}
		const all = `["structured_outputs","response_format","max_tokens","temperature"]`
		row := func(id, p, params string) string {
			return `{"model_id":"` + id + `","pricing":{"prompt":"` + p + `","completion":"0.000001"},"supported_parameters":` + params + `}`
		}
		_, _ = w.Write([]byte(`{"data":[` + strings.Join([]string{
			row("a/b", "0.000003", all), row("a/b", "0.000001", all), row("a/b", "0.000002", all),
			row("c/d", "0.000001", `["structured_outputs","response_format","max_tokens"]`),
		}, ",") + `]}`))
	}))
	defer srv.Close()
	store := newFakeStore()
	c := NewClient()
	c.base = srv.URL
	p := completePlugin(t, store, enforcingConfig(), c)
	for range 2 {
		got, err := p.Prices(context.Background(), "g1")
		if err != nil || len(got) != 1 || got["a/b"] != (core.ModelPrice{Prompt: 2, Completion: 1}) {
			t.Fatalf("%+v, %v", got, err)
		}
	}
	if reads != 1 {
		t.Errorf("read %d times", reads)
	}
	// What was read before outlives a gateway that stopped answering.
	srv.Close()
	p.now = func() time.Time { return testNow.Add(pricesTTL + time.Second) }
	if got, err := p.Prices(context.Background(), "g1"); err != nil || len(got) != 1 {
		t.Errorf("%+v, %v", got, err)
	}
}

// The lent pre-filter learns what another plugin's model always refuses,
// keeps it from aimod's own, and never confuses it with what was wanted.
func TestTheLentPreFilter(t *testing.T) {
	ctx := context.Background()
	p := testPlugin(t, newFakeStore(), &scriptedChat{}, newFakeOps(), &fakeAudit{})
	junk := []string{"blorp zeng wuvvle", "zeng zeng blorp frim", "wuvvle frim zorp blorp"}
	dutch := []string{"morgen gezellig samen eten", "kunnen jullie alsjeblieft stil blijven", "lege winkels vandaag gezien", "nederlanders houden van kaas"}
	if p.Unlike(ctx, "g1", "x", junk[0]) {
		t.Error("skipped before it learned anything")
	}
	for i := range lentWarmup {
		p.Teach(ctx, "g1", "x", junk[i%len(junk)], false)
		p.Teach(ctx, "g1", "x", dutch[i%len(dutch)], true)
	}
	if !p.Unlike(ctx, "g1", "x", "zorp blorp zeng") {
		t.Error("never learned to skip")
	}
	for _, s := range append(dutch, "jullie eten vandaag kaas", "", "etwas ganz neues hier") {
		if p.Unlike(ctx, "g1", "x", s) {
			t.Errorf("%q was skipped", s)
		}
	}
	if p.Unlike(ctx, "g1", "y", "zorp blorp zeng") || p.triageFor(ctx, "g1").Stats().Examples != 0 {
		t.Error("one use taught another")
	}
	p.triageSample = func() bool { return true }
	if p.Unlike(ctx, "g1", "x", "zorp blorp zeng") {
		t.Error("the sampled share was skipped")
	}
	p.wg.Wait()
}
