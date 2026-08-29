package aimod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The model catalogue and the cost surfaces built on it.
//
// These run against a stub OpenRouter rather than a fake classifier, because
// the code under test asserts p.client to a *Client on purpose (see keyInfo's
// comment) and a fake would take the "unavailable in this build" branch and
// exercise nothing. The stub is the same shape openrouter_test.go already
// uses.

// stubCatalogue serves /models and /key, and returns a plugin wired to it.
func stubCatalogue(t *testing.T, store *fakeStore) *Plugin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(`{"data":[
				{"id":"openai/gpt-5-nano","name":"GPT-5 Nano","context_length":400000,
				 "pricing":{"prompt":"0.00000005","completion":"0.0000004"}},
				{"id":"openai/gpt-5-mini","name":"GPT-5 Mini","context_length":400000,
				 "pricing":{"prompt":"0.00000025","completion":"0.000002"}},
				{"id":"free/model","name":"Free Model","context_length":8192,
				 "pricing":{"prompt":"0","completion":"0"}}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/key"):
			_, _ = w.Write([]byte(`{"data":{"label":"merlin","usage":1.5,"usage_daily":0.2,"is_free_tier":false}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client := NewClient()
	client.base = srv.URL

	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.client = client

	sealer, err := newSealer(testSecretKey)
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	p.sealer = sealer
	sealed, err := sealer.seal("sk-or-v1-test")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	cfg := enforcingConfig()
	cfg.APIKeySealed = sealed
	store.setConfig(cfg)
	return p
}

// The catalogue is cached per guild so autocomplete does not make a network
// call per keystroke, which is the whole reason modelCache exists.
func TestCatalogueIsFetchedOnceAndCached(t *testing.T) {
	p := stubCatalogue(t, newFakeStore())
	ctx := context.Background()

	first, err := p.catalogue(ctx, "g1")
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("got %d models, want 3", len(first))
	}
	if m, ok := findModel(first, "openai/gpt-5-nano"); !ok || m.PromptPerM == 0 {
		t.Errorf("prices did not parse: %+v", m)
	}
	if m, ok := findModel(first, "free/model"); !ok || !m.Free {
		t.Errorf("a zero-priced model was not marked free: %+v", m)
	}

	// Second read is served from the cache. Break the base URL to prove it:
	// a cache miss would now fail.
	p.client.(*Client).base = "http://127.0.0.1:1"
	if _, err := p.catalogue(ctx, "g1"); err != nil {
		t.Errorf("second catalogue read went back to the network: %v", err)
	}

	// And a guild the bot has left drops its entry, so the process is not
	// holding a copy of that guild's key configuration.
	p.models.forgetGuild("g1")
	if _, ok := p.models.get("g1", time.Now().UTC()); ok {
		t.Error("forgetGuild left the entry in place")
	}
}

// No key is a working state for this plugin, so asking for prices without one
// has to say so rather than fail in a way a handler reports as a bug.
func TestCatalogueWithoutAKeySaysSo(t *testing.T) {
	store := newFakeStore()
	p := stubCatalogue(t, store)
	cfg := enforcingConfig()
	cfg.APIKeySealed = nil
	store.setConfig(cfg)
	p.models.forgetGuild("g1")

	if _, err := p.catalogue(context.Background(), "g1"); err == nil {
		t.Error("asking for prices with no key configured was reported as success")
	}
}

// The comma-ok assertions in catalogue, keyInfo and reasoningLine exist so a
// build whose classifier is not a *Client degrades to a message instead of
// panicking inside a command handler, where the router's recover() would turn
// it into "the application did not respond" with no clue why.
func TestPriceSurfacesDegradeWithoutARealClient(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	sealer, _ := newSealer(testSecretKey)
	p.sealer = sealer

	if _, err := p.keyInfo(context.Background(), "k"); err == nil {
		t.Error("keyInfo did not report that prices are unavailable in this build")
	}
	// reasoningLine degrades to saying nothing rather than to a wrong claim:
	// whether an endpoint bills for thinking is learned from that endpoint
	// refusing to switch it off, and a build with no real client has never
	// asked, so it has no answer to give.
	if line := p.reasoningLine([]string{"a/b"}); line != "" {
		t.Errorf("reasoningLine = %q, want nothing when it cannot know", line)
	}
}

func TestKeyInfoReadsTheAccount(t *testing.T) {
	p := stubCatalogue(t, newFakeStore())
	info, err := p.keyInfo(context.Background(), "k")
	if err != nil {
		t.Fatalf("keyInfo: %v", err)
	}
	if info.Label != "merlin" || info.Usage != 1.5 {
		t.Errorf("key info = %+v, want the stubbed account", info)
	}
}

// stackLines renders the configured or default stack with prices beside it,
// and has to say which of the two it is showing: an admin who never ran
// set-fast is tracking whatever this build ships, and that is worth knowing.
func TestStackLinesNamesDefaultsAsDefaults(t *testing.T) {
	p := stubCatalogue(t, newFakeStore())
	catalogue, err := p.catalogue(context.Background(), "g1")
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}

	configured := stackLines([]string{"openai/gpt-5-mini"}, catalogue, []string{"openai/gpt-5-mini"})
	if !strings.Contains(configured, "gpt-5-mini") {
		t.Errorf("configured stack did not name its model:\n%s", configured)
	}

	defaults := stackLines(defaultFastModels, catalogue, nil)
	if !strings.Contains(strings.ToLower(defaults), "default") {
		t.Errorf("a default stack was not labelled as one:\n%s", defaults)
	}

	// A model the catalogue does not carry still renders, because a stack
	// entry that has been retired is exactly what an admin needs to see.
	unknown := stackLines([]string{"retired/model"}, catalogue, []string{"retired/model"})
	if !strings.Contains(unknown, "retired/model") {
		t.Errorf("a model missing from the catalogue vanished from the list:\n%s", unknown)
	}
}

// scaleHistory answers "what would this guild's traffic cost at a different
// volume". It scales the measured token and call counts, deliberately not
// SpentUSD: the estimate is rebuilt from live prices, so carrying the old
// dollar figure forward would quietly price a hypothetical against whatever
// the models cost last week.
func TestScaleHistoryScalesTokensNotTheOldBill(t *testing.T) {
	day := today(time.Now().UTC())
	history := []Spend{{
		Day: day, Scanned: 100, SpentUSD: 1.0,
		FastPromptTokens: 1000, DeepPromptTokens: 400, DeepCalls: 2,
	}}

	scaled := scaleHistory(history, 200)
	if len(scaled) != 1 {
		t.Fatalf("got %d days, want 1", len(scaled))
	}
	if scaled[0].Scanned != 200 {
		t.Errorf("scanned = %d, want 200 at double the volume", scaled[0].Scanned)
	}
	if scaled[0].FastPromptTokens != 2000 || scaled[0].DeepPromptTokens != 800 {
		t.Errorf("tokens did not scale with the volume: %+v", scaled[0])
	}
	if scaled[0].SpentUSD != history[0].SpentUSD {
		t.Error("the old dollar figure was scaled, pricing a hypothetical against last week's rates")
	}

	// A day nothing was scanned has no ratio to scale by, and must not
	// divide by zero.
	empty := scaleHistory([]Spend{{Day: day}}, 200)
	if len(empty) != 1 || empty[0].Scanned != 0 {
		t.Errorf("scaling a day with nothing scanned produced %+v", empty)
	}
	// And a nonsense volume is left alone rather than zeroing the history.
	if got := scaleHistory(history, 0); got[0].Scanned != 100 {
		t.Errorf("a volume of zero rewrote the history: %+v", got[0])
	}
}

func TestPriceLineHandlesAMissingModel(t *testing.T) {
	if line := priceLine("gone/model", Model{}, false); !strings.Contains(line, "gone/model") {
		t.Errorf("priceLine dropped the id of a model it could not price: %q", line)
	}
	line := priceLine("a/b", Model{ID: "a/b", PromptPerM: 0.05, CompletionPerM: 0.4}, true)
	if !strings.Contains(line, "a/b") {
		t.Errorf("priceLine = %q, want the model named", line)
	}
}

// The two read-only price surfaces, driven end to end. Both do real work
// behind a defer, so the assertion that matters is that they complete: a
// panic here reaches a user as "the application did not respond".
func TestModelsShowAndCompareRun(t *testing.T) {
	p := stubCatalogue(t, newFakeStore())
	s := testSession(t)

	p.handleModelsShow(context.Background(), s, interaction("g1", "models", "show"))
	p.handleModelsCompare(context.Background(), s, interaction("g1", "models", "compare",
		strOpt("model", "openai/gpt-5-mini"), strOpt("pass", "deep")))
	// A model nobody has heard of is an ordinary typo, not a failure.
	p.handleModelsCompare(context.Background(), s, interaction("g1", "models", "compare",
		strOpt("model", "nobody/knows"), strOpt("pass", "fast")))
}

// "default" is the documented way to go back to tracking whatever this build
// ships, rather than freezing a model ID chosen on the day someone ran the
// command.
func TestSetStackStoresAndResetsToDefault(t *testing.T) {
	store := newFakeStore()
	p := stubCatalogue(t, store)
	s := testSession(t)

	p.handleSetFast(context.Background(), s, interaction("g1", "models", "set-fast",
		strOpt("models", "openai/gpt-5-nano")))
	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.FastModels) != 1 || cfg.FastModels[0] != "openai/gpt-5-nano" {
		t.Errorf("fast stack = %v, want the model just set", cfg.FastModels)
	}

	p.handleSetDeep(context.Background(), s, interaction("g1", "models", "set-deep",
		strOpt("models", "openai/gpt-5-mini")))
	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.DeepModels) != 1 {
		t.Errorf("deep stack = %v, want the model just set", cfg.DeepModels)
	}

	p.handleSetFast(context.Background(), s, interaction("g1", "models", "set-fast",
		strOpt("models", "default")))
	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.FastModels) != 0 {
		t.Errorf("fast stack = %v, want empty so it tracks the compiled-in defaults", cfg.FastModels)
	}
}

// Discord rejects an autocomplete response over 25 choices outright, leaving
// the user with no suggestions rather than a truncated list. That is a hard
// protocol limit, so it is asserted rather than assumed.
func TestModelAutocompleteStaysUnderDiscordsLimit(t *testing.T) {
	p := stubCatalogue(t, newFakeStore())
	choices := p.autocompleteModel(context.Background(), interaction("g1", "models", "set-fast"), "models", "gpt")
	if len(choices) > 25 {
		t.Errorf("returned %d choices, past Discord's cap of 25", len(choices))
	}
	for _, c := range choices {
		if !strings.Contains(strings.ToLower(c.Value.(string)), "gpt") {
			t.Errorf("choice %q does not match the typed prefix", c.Value)
		}
	}
}

// A guild with no key configured cannot be offered prices, and autocomplete
// is the one surface where failing loudly is not an option: it runs on every
// keystroke and has nowhere to put an error.
func TestModelAutocompleteIsSilentWithoutAKey(t *testing.T) {
	store := newFakeStore()
	p := stubCatalogue(t, store)
	cfg := enforcingConfig()
	cfg.APIKeySealed = nil
	store.setConfig(cfg)
	p.models.forgetGuild("g1")

	if got := p.autocompleteModel(context.Background(), interaction("g1", "models", "set-fast"), "models", "gpt"); len(got) != 0 {
		t.Errorf("returned %d choices with no key configured", len(got))
	}
}

var _ = discordgo.ApplicationCommandOptionChoice{}
