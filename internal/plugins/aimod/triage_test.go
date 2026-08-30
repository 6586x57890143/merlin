package aimod

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Sentences that stand in for a server's ordinary traffic and for the small
// share of it the fast pass flags. Deliberately unremarkable: the point is
// that a linear model over character n-grams separates them at all, not that
// these particular strings are interesting.
var (
	triageClean = []string{
		"anyone want to play later tonight",
		"the new patch notes just dropped",
		"lol that was a good round",
		"i will be back in about ten minutes",
		"does anyone have a link to the vod",
		"good morning everyone hope you slept well",
		"that boss fight took me four tries",
		"i am making dinner brb",
	}
	triageFlagged = []string{
		"kill yourself you worthless waste",
		"i hope you die in a fire",
		"you should just end it already nobody wants you",
		"go slit your wrists loser",
	}
)

func trainTriage(m *triageModel, rounds int) {
	for r := 0; r < rounds; r++ {
		for _, s := range triageClean {
			m.Learn(triageFeatures(s), false)
		}
		for _, s := range triageFlagged {
			m.Learn(triageFeatures(s), true)
		}
	}
}

// The whole rung rests on this: after seeing a server's own traffic the model
// has to put ordinary chatter below the skip threshold and anything like a
// flagged message well above it. If this fails the rung is either useless
// (skips nothing) or dangerous (skips everything).
func TestTriageModelSeparatesCleanFromFlagged(t *testing.T) {
	m := newTriageModel()
	trainTriage(m, 60)

	for _, s := range triageClean {
		if got := m.Score(triageFeatures(s)); got >= triageSkipThreshold {
			t.Errorf("clean text scored %.4f, above the skip threshold %.4f: %q", got, triageSkipThreshold, s)
		}
	}
	for _, s := range triageFlagged {
		if got := m.Score(triageFeatures(s)); got < triageSkipThreshold {
			t.Errorf("flagged text scored %.4f, low enough to be skipped: %q", got, s)
		}
	}
}

// Generalisation rather than memorisation. A model that only recognises the
// exact strings it was trained on would skip every new violation, which is
// the failure that matters here: character n-grams are chosen precisely so a
// rephrasing lands near the phrasings already seen.
func TestTriageModelGeneralisesToUnseenText(t *testing.T) {
	m := newTriageModel()
	trainTriage(m, 60)

	unseenClean := "anyone around to play a few rounds this evening"
	if got := m.Score(triageFeatures(unseenClean)); got >= triageSkipThreshold {
		t.Errorf("unseen ordinary text scored %.4f, so nothing new would ever be skipped", got)
	}
	unseenFlagged := "kill yourself already nobody wants you here"
	if got := m.Score(triageFeatures(unseenFlagged)); got < triageSkipThreshold {
		t.Errorf("unseen abusive text scored %.4f, low enough to skip: it would never reach a model", got)
	}
}

// The imbalance trap. Roughly one message in a hundred is flagged, so the
// loss is minimised by answering "clean" to everything: that model is right
// 99% of the time and skips every violation on the server. triagePosWeight is
// what buys precision on the answer that actually matters, and this pins it.
func TestTriageDoesNotCollapseToAlwaysCleanUnderImbalance(t *testing.T) {
	m := newTriageModel()
	for r := 0; r < 200; r++ {
		for _, s := range triageClean {
			m.Learn(triageFeatures(s), false)
		}
		// One flagged example per 8 clean ones, and only every fifth round,
		// so the positives are about 2.5% of the stream.
		if r%5 == 0 {
			m.Learn(triageFeatures(triageFlagged[r%len(triageFlagged)]), true)
		}
	}
	for _, s := range triageFlagged {
		if got := m.Score(triageFeatures(s)); got < triageSkipThreshold {
			t.Fatalf("under imbalance the model collapsed to always-clean: %q scored %.4f", s, got)
		}
	}
}

// An untrained model is never allowed to act, whatever it happens to think.
// This is what makes a fresh guild, a new deployment and a restored backup all
// behave exactly as the plugin did before this rung existed.
func TestTriageSkipsNothingBeforeWarmup(t *testing.T) {
	p, cfg := triagePlugin(t, TriageOn)
	m := p.triageFor(context.Background(), cfg.GuildID)
	trainTriage(m, 5) // well short of triageWarmup

	if m.Ready() {
		t.Fatalf("model reports ready at %d examples, warmup is %d", m.Stats().Examples, triageWarmup)
	}
	if d := p.triageDecide(context.Background(), cfg, triageClean[0]); d.skip || d.wouldSkip {
		t.Fatalf("an unwarmed model was allowed to skip: %+v", d)
	}
}

// The one bucket where the cheap answer is never the final one. Every other
// force in this package pushes toward leniency, and Discord's policy has no
// humour exception, so the model's confidence is overridden rather than
// trusted. The override is one way: it only ever causes a normal scan.
func TestTriageNeverSkipsChildSafetyVocabulary(t *testing.T) {
	p, cfg := triagePlugin(t, TriageOn)
	m := p.triageFor(context.Background(), cfg.GuildID)
	trainTriage(m, 80)

	// Trained to look utterly ordinary, so only the veto can be what stops it.
	vetoed := "anyone want to play later tonight with my 12 year old cousin"
	for r := 0; r < 40; r++ {
		m.Learn(triageFeatures(vetoed), false)
	}
	if got := m.Score(triageFeatures(vetoed)); got >= triageSkipThreshold {
		t.Fatalf("fixture is wrong: the model does not find %q clean (scored %.4f)", vetoed, got)
	}
	if d := p.triageDecide(context.Background(), cfg, vetoed); d.skip || d.wouldSkip {
		t.Fatalf("a message the model was confident about was skipped despite the child-safety veto: %+v", d)
	}

	for _, s := range []string{
		"is she a minor",
		"that guy is into underage stuff",
		"he said he was 15 years old",
		"talking about kids again",
	} {
		if !mustScan(s) {
			t.Errorf("mustScan(%q) = false, so a confident model could skip it", s)
		}
	}
	// And the veto does not swallow the whole server: ordinary traffic is
	// still eligible to be skipped, or the rung saves nothing.
	for _, s := range triageClean {
		if mustScan(s) {
			t.Errorf("mustScan(%q) = true, the veto is too broad to leave any saving", s)
		}
	}
}

// Shadow scores and learns and changes nothing. It exists so an admin can read
// a real number before trusting the rung with their server's coverage, which
// only works if shadow genuinely never skips.
func TestTriageShadowModeChangesNothing(t *testing.T) {
	p, cfg := triagePlugin(t, TriageShadow)
	m := p.triageFor(context.Background(), cfg.GuildID)
	trainTriage(m, 80)

	d := p.triageDecide(context.Background(), cfg, triageClean[0])
	if d.skip {
		t.Fatal("shadow mode skipped a message")
	}
	if !d.wouldSkip {
		t.Fatal("shadow mode recorded no opinion, so /aimod status would show nothing to decide on")
	}
	st := m.Stats()
	if st.Skipped != 0 {
		t.Errorf("shadow mode counted %d skips", st.Skipped)
	}
	if st.WouldSkip == 0 || st.Considered == 0 {
		t.Errorf("shadow mode kept no score: %+v", st)
	}
}

// Off means off: no score, no opinion, no counters moving.
func TestTriageOffModeDoesNothing(t *testing.T) {
	p, cfg := triagePlugin(t, TriageOff)
	m := p.triageFor(context.Background(), cfg.GuildID)
	trainTriage(m, 80)

	if d := p.triageDecide(context.Background(), cfg, triageClean[0]); d.skip || d.wouldSkip {
		t.Fatalf("an off rung formed an opinion: %+v", d)
	}
	if st := m.Stats(); st.Considered != 0 {
		t.Errorf("an off rung was still counting: %+v", st)
	}
}

// End to end: a message the rung is confident about never reaches a model.
// This is the saving the whole rung exists for.
func TestTriageOnSkipsTheModelCall(t *testing.T) {
	client := &fakeClassifier{}
	store := newFakeStore()
	p := intakePlugin(t, store, client, newFakeOps())

	cfg, _ := store.Config(context.Background(), "g1")
	cfg.TriageMode = TriageOn
	store.setConfig(cfg)

	trainTriage(p.triageFor(context.Background(), "g1"), 80)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: triageClean[0],
		Author:  &discordgo.User{ID: "u1", Username: "someguy"},
	})
	p.flush("c1")
	p.wg.Wait()

	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("a message the local rung cleared still cost a model call: fast=%d deep=%d", fast, deep)
	}
}

// The counterpart: something the rung is not confident about still goes to a
// model exactly as before. A rung that skipped everything would pass the test
// above and be useless.
func TestTriagePassesThroughWhatItIsUnsureOf(t *testing.T) {
	client := &fakeClassifier{}
	store := newFakeStore()
	p := intakePlugin(t, store, client, newFakeOps())

	cfg, _ := store.Config(context.Background(), "g1")
	cfg.TriageMode = TriageOn
	store.setConfig(cfg)

	trainTriage(p.triageFor(context.Background(), "g1"), 80)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: triageFlagged[0],
		Author:  &discordgo.User{ID: "u1", Username: "someguy"},
	})
	p.flush("c1")
	p.wg.Wait()

	if fast, _ := client.counts(); fast == 0 {
		t.Error("a message the local rung was unsure about never reached the fast pass")
	}
}

// A skipped message was never scanned, so it must not draw on the member's
// scan ceiling. Charging for a call that was never made would let ordinary
// chatter exhaust the quota that exists for content nobody has judged.
func TestTriageSkipDoesNotSpendTheScanCeiling(t *testing.T) {
	p, cfg := triagePlugin(t, TriageOn)
	trainTriage(p.triageFor(context.Background(), cfg.GuildID), 80)

	for i := 0; i < maxUserScans*2; i++ {
		if d := p.triageDecide(context.Background(), cfg, triageClean[i%len(triageClean)]); !d.skip {
			t.Fatalf("fixture is wrong: message %d was not skipped", i)
		}
	}

	// The ceiling is untouched, so a member whose ordinary chatter was all
	// skipped still has their whole quota for anything that does need judging.
	// Counted by spending it: the meter has no read-only view, and the number
	// that matters is how many real scans are still available.
	left := 0
	for p.meter.allowScan(cfg.GuildID, "u1", testNow) {
		left++
		if left > maxUserScans {
			break
		}
	}
	if left != maxUserScans {
		t.Errorf("%d scans left of %d: skipped messages spent the ceiling", left, maxUserScans)
	}
}

// Weights survive a restart, so a deploy does not cost a fresh warmup. This
// bot redeploys on every push to main, so a model that never persisted would
// be cold most of the time and save nothing.
func TestTriageModelRoundTrips(t *testing.T) {
	m := newTriageModel()
	trainTriage(m, 60)
	want := m.Score(triageFeatures(triageFlagged[0]))

	raw, examples := encodeTriageModel(m)
	restored, ok := decodeTriageModel(raw, examples)
	if !ok {
		t.Fatal("a model this build wrote could not be read back")
	}
	if got := restored.Score(triageFeatures(triageFlagged[0])); got != want {
		t.Errorf("restored score = %v, want %v", got, want)
	}
	if restored.Stats().Examples != m.Stats().Examples {
		t.Errorf("restored example count = %d, want %d", restored.Stats().Examples, m.Stats().Examples)
	}
}

// A blob of the wrong length means the table size changed between releases.
// Reinterpreting old weights against a new hash space gives a model that is
// confidently wrong about everything, which is far worse than starting again:
// starting again is just a warmup during which nothing is skipped.
func TestTriageModelRefusesAMismatchedBlob(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		make([]byte, 16),
		make([]byte, 4*triageBuckets),   // missing the bias
		make([]byte, 4*triageBuckets+9), // one byte too many
	} {
		if _, ok := decodeTriageModel(raw, 1000); ok {
			t.Errorf("a %d byte blob was accepted as a model", len(raw))
		}
	}
}

func TestTriageFeaturesNormalizeAndBound(t *testing.T) {
	// Case and punctuation are noise: these should share most of their
	// features, which is what lets the model generalise over the spellings
	// people actually type.
	a := triageFeatures("K Y S !!!")
	b := triageFeatures("k y s")
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("normalisation dropped everything")
	}
	shared := 0
	inA := map[uint32]bool{}
	for _, f := range a {
		inA[f] = true
	}
	for _, f := range b {
		if inA[f] {
			shared++
		}
	}
	if shared == 0 {
		t.Error("case and punctuation produced entirely disjoint features")
	}

	// Bounded, because this runs on the gateway goroutine for every message.
	if got := triageFeatures(strings.Repeat("abcdefgh ", 4000)); len(got) > triageMaxFeatures {
		t.Errorf("a long paste produced %d features, cap is %d", len(got), triageMaxFeatures)
	}
	// Features are presence, not counts, so a repeated n-gram appears once.
	feats := triageFeatures("aaaa aaaa aaaa aaaa")
	seen := map[uint32]bool{}
	for _, f := range feats {
		if seen[f] {
			t.Fatal("a duplicate feature index was emitted")
		}
		seen[f] = true
	}
	if triageFeatures("") != nil || triageFeatures("   ") != nil {
		t.Error("empty text should produce no features, so the rung abstains")
	}
}

// A saturated score is the normal state for a confident linear model, and
// math.Exp of a large number is +Inf, which turns the gradient into a NaN that
// then poisons every weight it touches.
func TestSigmoidDoesNotOverflow(t *testing.T) {
	for _, z := range []float64{-1e9, -100, 0, 100, 1e9} {
		got := sigmoid(z)
		if got != got || got < 0 || got > 1 {
			t.Errorf("sigmoid(%v) = %v, want a probability", z, got)
		}
	}
	// And a model pushed hard in one direction still learns, rather than
	// producing NaN weights that make every later score meaningless.
	m := newTriageModel()
	for i := 0; i < 2000; i++ {
		m.Learn(triageFeatures(triageFlagged[0]), true)
	}
	if got := m.Score(triageFeatures(triageFlagged[0])); got != got {
		t.Fatal("weights went to NaN under repeated one-sided training")
	}
}

// Weights are clipped in place of a regularisation term, so one n-gram from
// one heated afternoon cannot come to dominate every score.
func TestTriageWeightsStayClipped(t *testing.T) {
	m := newTriageModel()
	for i := 0; i < 3000; i++ {
		m.Learn(triageFeatures(triageFlagged[0]), true)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, w := range m.w {
		if w > triageWeightClip || w < -triageWeightClip {
			t.Fatalf("weight %d = %v, outside the clip of %v", i, w, triageWeightClip)
		}
	}
}

// The rung learns from the fast pass's own verdict, which is the only training
// input it has. Nothing is stored to train from later.
func TestTriageLearnsFromTheFastPassVerdict(t *testing.T) {
	p, cfg := triagePlugin(t, TriageShadow)
	ctx := context.Background()

	before := p.triageFor(ctx, cfg.GuildID).Stats().Examples
	p.triageLearn(ctx, cfg, "some ordinary chatter here", false)
	p.triageLearn(ctx, cfg, triageFlagged[0], true)
	after := p.triageFor(ctx, cfg.GuildID).Stats().Examples

	if after != before+2 {
		t.Fatalf("examples went %d -> %d, want two learned", before, after)
	}
	// An off guild learns nothing, so turning the rung off really does stop
	// it rather than merely muting it.
	off := cfg
	off.TriageMode = TriageOff
	p.triageLearn(ctx, off, triageFlagged[0], true)
	if got := p.triageFor(ctx, cfg.GuildID).Stats().Examples; got != after {
		t.Errorf("an off rung kept learning: %d", got)
	}
}

// Sampling is what stops the rung eating itself: without it the model stops
// receiving labels from the region it skips, and any later drift is invisible
// exactly where it is trusted most. A sampled message is scanned and its
// outcome counted.
func TestTriageSamplingScansAnywayAndCountsAMiss(t *testing.T) {
	p, cfg := triagePlugin(t, TriageOn)
	ctx := context.Background()
	m := p.triageFor(ctx, cfg.GuildID)
	trainTriage(m, 80)

	p.triageSample = func() bool { return true }
	d := p.triageDecide(ctx, cfg, triageClean[0])
	if d.skip {
		t.Fatal("a sampled message was skipped rather than scanned")
	}
	if !d.sampled || !d.wouldSkip {
		t.Fatalf("a sampled message was not recorded as one: %+v", d)
	}
	if st := m.Stats(); st.Sampled != 1 || st.Skipped != 0 {
		t.Fatalf("sampling accounting is wrong: %+v", st)
	}

	p.triageMiss(ctx, cfg)
	if st := m.Stats(); st.Missed != 1 {
		t.Errorf("a miss was not counted: %+v", st)
	}
}

// triagePlugin builds a plugin whose guild is in the given triage mode.
func triagePlugin(t *testing.T, mode TriageMode) (*Plugin, Config) {
	t.Helper()
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	cfg := enforcingConfig()
	cfg.TriageMode = mode
	store.setConfig(cfg)
	return p, cfg
}
