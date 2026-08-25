package aimod

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The ladder's safety property, stated once so it can be broken loudly: a
// fast-pass hit on its own may only ever flag. Deletion and rewriting need
// the deep pass to have read the message against the full policy text and
// answered above actThreshold. Everything in this file exists to keep that
// true, because it is the single rule standing between a nano model's
// false positives and a free-speech server's messages.

func enforcingConfig() Config {
	return Config{
		GuildID:        "g1",
		Mode:           ModeEnforce,
		APIKeySealed:   []byte("sealed"),
		DailyBudgetUSD: 1,
		EvidenceHours:  24,
		BucketActions:  map[Bucket]Action{},
	}
}

// classifyOne drives the paid rungs synchronously for one message, which is
// what the batch path does once its timer has fired.
func (p *Plugin) classifyOne(t testingT, cfg Config, c candidate) {
	t.Helper()
	hits, _, err := p.classifyFast(context.Background(), "key", cfg, []candidate{c})
	if err != nil {
		return
	}
	for _, hit := range hits {
		p.escalate(context.Background(), cfg, "key", c, hit)
	}
}

func TestFastPassAloneNeverDeletes(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	audit := &fakeAudit{}
	client := &fakeClassifier{
		// A confident fast-pass hit on a bucket configured to remove.
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.99}]}`},
		// The deep pass clears it, which is the normal outcome and the one
		// the whole two-tier design exists to make cheap.
		deep: []string{`{"violation":false,"bucket":"threats","confidence":0.1,"reason":"hyperbole, no real target"}`},
	}
	p := testPlugin(t, store, client, ops, audit)

	p.classifyOne(t, enforcingConfig(), candidate{
		MessageID: "m1", ChannelID: "c1", AuthorID: "u1",
		Content: "i will absolutely end you at mario kart tonight",
	})

	deleted, _ := ops.snapshot()
	if len(deleted) != 0 {
		t.Errorf("deleted %v on a fast-pass hit the deep pass cleared", deleted)
	}
	if fast, deep := client.counts(); fast != 1 || deep != 1 {
		t.Errorf("calls: fast=%d deep=%d, want 1 and 1", fast, deep)
	}
	if len(store.recorded()) != 0 {
		t.Error("recorded an incident for a message the deep pass cleared")
	}
}

func TestDeepConfirmationDeletes(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.8}]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"a specific person, a specific act"}`},
	}
	p := testPlugin(t, store, client, ops, &fakeAudit{})

	p.classifyOne(t, enforcingConfig(), candidate{
		MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "some genuinely threatening sentence",
	})

	deleted, _ := ops.snapshot()
	if len(deleted) != 1 || deleted[0] != "m1" {
		t.Errorf("deleted = %v, want [m1]", deleted)
	}
	inc := store.recorded()
	if len(inc) != 1 {
		t.Fatalf("recorded %d incidents, want 1", len(inc))
	}
	if inc[0].Action != ActionRemove || inc[0].Bucket != BucketThreats {
		t.Errorf("incident = %q/%q, want remove/threats", inc[0].Action, inc[0].Bucket)
	}
	if inc[0].Content == "" {
		t.Error("no evidence stored, so /aimod undo cannot reverse this")
	}
}

// Below actThreshold the deep pass has said "probably, but I am not sure",
// and this filter does not act on probably.
func TestLowConfidenceDeepVerdictDoesNotAct(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.6,"reason":"maybe"}`},
	}
	p := testPlugin(t, store, client, ops, &fakeAudit{})

	p.classifyOne(t, enforcingConfig(), candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "borderline"})

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v at confidence %.2f, below actThreshold %.2f", deleted, 0.6, actThreshold)
	}
}

// A model that is unreachable, rate limited or returning nonsense has not
// said a message is fine and has not said it is a violation. Treating that
// as a confirmation would let an outage delete messages.
func TestDeepPassFailureActsOnNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *fakeClassifier
	}{
		{"deep pass errors", &fakeClassifier{
			fast:    []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
			deepErr: errors.New("openrouter: HTTP 503"),
		}},
		{"deep pass returns unparseable text", &fakeClassifier{
			fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
			deep: []string{"I am sorry, I cannot help with that."},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			ops := newFakeOps()
			p := testPlugin(t, store, tc.client, ops, &fakeAudit{})

			p.classifyOne(t, enforcingConfig(), candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "whatever"})

			if deleted, _ := ops.snapshot(); len(deleted) != 0 {
				t.Errorf("deleted %v after a failed deep pass", deleted)
			}
		})
	}
}

// A model can and does answer with an index outside the batch, a bucket that
// does not exist, or a bucket this guild switched off. Every one of those
// would otherwise become an incident against the wrong message or the wrong
// policy, so they are filtered here rather than trusted.
func TestFastPassOutputIsNotTrusted(t *testing.T) {
	cfg := enforcingConfig()
	cfg.BucketActions[BucketHateSpeech] = ActionOff

	client := &fakeClassifier{fast: []string{`{"v":[
		{"i":0,"b":"threats","c":0.9},
		{"i":9,"b":"threats","c":0.9},
		{"i":1,"b":"invented_bucket","c":0.9},
		{"i":1,"b":"hate_speech","c":0.9},
		{"i":1,"b":"threats","c":0.1}
	]}`}}
	p := testPlugin(t, newFakeStore(), client, newFakeOps(), &fakeAudit{})

	hits, _, err := p.classifyFast(context.Background(), "key", cfg, []candidate{{MessageID: "m1", Content: "one message"}})
	if err != nil {
		t.Fatalf("classifyFast: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want none: every entry was out of range, unknown, disabled or below deepThreshold", hits)
	}
}

// Nothing the model says may redirect the deep pass onto a different policy
// from the one it was handed. Letting it would mean a bucket a guild
// switched off could still get a message deleted.
func TestDeepVerdictBucketIsPinned(t *testing.T) {
	client := &fakeClassifier{
		deep: []string{`{"violation":true,"bucket":"hate_speech","confidence":0.99,"reason":"r"}`},
	}
	p := testPlugin(t, newFakeStore(), client, newFakeOps(), &fakeAudit{})

	v, _, err := p.classifyDeep(context.Background(), "key", enforcingConfig(), BucketThreats,
		candidate{MessageID: "m1", Content: "x"}, nil, false)
	if err != nil {
		t.Fatalf("classifyDeep: %v", err)
	}
	if v.Bucket != BucketThreats {
		t.Errorf("bucket = %q, want threats: the model answered about a policy nobody asked it about", v.Bucket)
	}
}

// The fast prompt is sent on every scanned batch, so what goes into it is
// the cost of the whole feature. It carries one line per enforced bucket and
// must not carry the full policy files.
func TestFastPromptCarriesOnlyShortLines(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	prompt := p.fastPrompt([]Bucket{BucketThreats, BucketDoxxing})

	if !strings.Contains(prompt, p.policies[BucketThreats].Short) {
		t.Error("the threats short line is missing from the fast prompt")
	}
	if strings.Contains(prompt, p.policies[BucketThreats].NotViolations[0]) {
		t.Error("a full policy boundary line reached the fast prompt, which pays for it on every batch")
	}
	if strings.Contains(prompt, string(BucketHateSpeech)) {
		t.Error("a bucket that was not enforced is being described to the model anyway")
	}
	// The framing that stops a general purpose model reading "hate speech"
	// as "rude" on a server built for arguing.
	if !strings.Contains(prompt, "not a civility filter") {
		t.Error("the fast prompt has lost its free-speech framing")
	}
}

// The deep prompt is the only place a full policy file is sent, and it is
// sent for exactly the one bucket that was flagged.
func TestDeepPromptCarriesOneWholePolicy(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	prompt := p.deepPrompt(BucketDoxxing, false)

	pol := p.policies[BucketDoxxing]
	if !strings.Contains(prompt, pol.Violations[0]) {
		t.Error("the doxxing violations list is missing from the deep prompt")
	}
	if !strings.Contains(prompt, pol.NotViolations[0]) {
		t.Error("the doxxing boundary is missing, which is the half that stops over-removal")
	}
	if strings.Contains(prompt, p.policies[BucketGore].Violations[0]) {
		t.Error("an unrelated policy file reached the deep prompt")
	}
	if strings.Contains(prompt, "rewrite (") {
		t.Error("a rewrite was requested for a removal, which pays for output tokens nothing reads")
	}
	if !strings.Contains(p.deepPrompt(BucketDoxxing, true), "rewrite (") {
		t.Error("no rewrite was requested for a rewrite action")
	}
}

func TestSanitizeForPromptBoundsAndFlattens(t *testing.T) {
	long := strings.Repeat("a", maxPromptChars+500)
	got := sanitizeForPrompt("line one\nline two\r\nline three " + long)

	if strings.ContainsAny(got, "\n\r") {
		t.Error("newlines survived, so a message can forge the numbered structure around it")
	}
	if len([]rune(got)) > maxPromptChars+len(" [truncated]") {
		t.Errorf("length %d, want it bounded near %d", len([]rune(got)), maxPromptChars)
	}
}

func TestModelsOrFallsBackToDefaults(t *testing.T) {
	if got := modelsOr(nil, defaultFastModels); len(got) != len(defaultFastModels) {
		t.Errorf("an unconfigured stack did not fall back to the defaults: %v", got)
	}
	if got := modelsOr([]string{"a/b"}, defaultFastModels); len(got) != 1 || got[0] != "a/b" {
		t.Errorf("a configured stack was overridden by the defaults: %v", got)
	}
}

// Both stacks are sent as OpenRouter fallback arrays and pinned to
// zero-retention providers. The provider block is the privacy promise this
// package makes, enforced per request rather than trusted to model choice.
func TestEveryRequestPinsPrivacy(t *testing.T) {
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
		deep: []string{`{"violation":false,"bucket":"threats","confidence":0.1,"reason":"r"}`},
	}
	p := testPlugin(t, newFakeStore(), client, newFakeOps(), &fakeAudit{})
	p.classifyOne(t, enforcingConfig(), candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "x"})

	for name, req := range map[string]chatRequest{"fast": client.lastFastReq, "deep": client.lastDeepReq} {
		if !req.Provider.ZDR {
			t.Errorf("%s pass did not require zero data retention", name)
		}
		if req.Provider.DataCollection != "deny" {
			t.Errorf("%s pass allowed provider data collection", name)
		}
		if !req.Provider.RequireParameters {
			t.Errorf("%s pass did not require schema support, so a provider may ignore the JSON schema", name)
		}
		if req.Temperature != 0 {
			t.Errorf("%s pass ran at temperature %v: a classifier must answer the same twice", name, req.Temperature)
		}
		if req.MaxTokens == 0 {
			t.Errorf("%s pass has no token ceiling", name)
		}
	}
}
