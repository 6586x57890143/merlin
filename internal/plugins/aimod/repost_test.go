package aimod

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core"
)

const dutch = "ik kom je vanavond opzoeken en dan doe ik je pijn"

// dutchMessage is the original every test here reposts.
var dutchMessage = candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: dutch}

func reposting(text string) core.Repost {
	return core.Repost{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Original: dutch, Text: text, Marker: "\n-# edited by merlin",
	}
}

func repostPlugin(t *testing.T) (*Plugin, *fakeStore, *fakeOps) {
	t.Helper()
	store, ops := newFakeStore(), newFakeOps()
	store.setConfig(enforcingConfig())
	ops.live = map[string]string{"m1": dutch}
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, store, ops
}

func mustRepost(t *testing.T, p *Plugin, r core.Repost) {
	t.Helper()
	if id, err := p.Repost(context.Background(), r); err != nil || id != "repost1" {
		t.Fatalf("Repost = %q, %v", id, err)
	}
}

func unavailable(err error) bool {
	var u interface{ Unavailable() bool }
	return errors.As(err, &u) && u.Unavailable()
}

func TestRepostPostsAsTheAuthorThenDeletes(t *testing.T) {
	p, _, ops := repostPlugin(t)
	mustRepost(t, p, reposting("hello there"))
	deleted, posted := ops.snapshot()
	if len(posted) != 1 || posted[0].Content != "hello there\n-# edited by merlin" || posted[0].Username != "memberu1" {
		t.Errorf("posted %+v", posted)
	}
	if !slices.Equal(deleted, []string{"m1"}) {
		t.Errorf("deleted %v", deleted)
	}
}

// The bypass this file exists to close: a verdict that lands after the
// repost has to find it.
func TestAVerdictOnTheOriginalFollowsTheRepost(t *testing.T) {
	p, store, ops := repostPlugin(t)
	cfg := enforcingConfig()
	cfg.SanctionAction = SanctionJail
	store.setConfig(cfg)
	jailer := &fakeJailer{}
	p.WithJailer(jailer)
	mustRepost(t, p, reposting("I'm coming to see you tonight"))

	p.enforce(context.Background(), cfg, dutchMessage, BucketThreats, ActionRemove, confirmed("r"))
	// And the copy's own verdict, which must not sentence them twice.
	again := dutchMessage
	again.MessageID = "repost1"
	p.enforce(context.Background(), cfg, again, BucketThreats, ActionRemove, confirmed("r"))

	deleted, _ := ops.snapshot()
	if !slices.Equal(deleted, []string{"m1", "repost1"}) {
		t.Errorf("deleted %v, want the original by the repost and the copy by the verdict", deleted)
	}
	if n := len(jailer.jailed()); n != 1 {
		t.Errorf("jailed %d times, want once", n)
	}
}

// The other order: what moderation took down is not put back.
func TestARemovedMessageIsNotReposted(t *testing.T) {
	p, _, ops := repostPlugin(t)
	delete(ops.live, "m1")
	if _, err := p.Repost(context.Background(), reposting("hello")); !unavailable(err) {
		t.Errorf("err = %v", err)
	}
	// A kept original has no delete to fail, so this check is all there is.
	r := reposting("hello")
	r.Keep = true
	if _, err := p.Repost(context.Background(), r); !unavailable(err) {
		t.Errorf("kept: err = %v", err)
	}
	ops.live["m1"] = "actually, nothing"
	if _, err := p.Repost(context.Background(), reposting("hello")); !unavailable(err) {
		t.Errorf("edited: err = %v", err)
	}
	if _, posted := ops.snapshot(); len(posted) != 0 {
		t.Errorf("posted %+v", posted)
	}
}

// Failing halfway costs the member nothing: the original stays.
func TestAFailedRepostLeavesTheOriginal(t *testing.T) {
	p, _, ops := repostPlugin(t)
	ops.postErr = errors.New("webhook gone")
	if _, err := p.Repost(context.Background(), reposting("hello")); err == nil || unavailable(err) {
		t.Errorf("err = %v", err)
	}
	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v though nothing was posted", deleted)
	}
	ops.postErr, ops.deleteErr = nil, unknownMessageError()
	if _, err := p.Repost(context.Background(), reposting("hello")); !unavailable(err) {
		t.Errorf("deleted meanwhile: err = %v", err)
	}
	if p.moved("m1") != nil {
		t.Error("a repost that did not happen is being followed")
	}
}

func TestRepostRefusesWhatCannotBePublished(t *testing.T) {
	p, _, ops := repostPlugin(t)
	for _, text := range []string{
		"", strings.Repeat("a", maxContentLen),
		"look https://dlscord-nitro.ru/free",
	} {
		if _, err := p.Repost(context.Background(), reposting(text)); !unavailable(err) {
			t.Errorf("%.30q: err = %v", text, err)
		}
	}
	if _, posted := ops.snapshot(); len(posted) != 0 {
		t.Errorf("posted %+v", posted)
	}
	p.WithGate(gateOff{})
	if _, err := p.Repost(context.Background(), reposting("hi")); !errors.Is(err, ErrNoKey) {
		t.Errorf("disabled: err = %v", err)
	}
}

// The house list and the slur swap apply to what is published, as they do to
// a rewrite.
func TestRepostCleansWhatItPublishes(t *testing.T) {
	p, store, ops := repostPlugin(t)
	cfg := enforcingConfig()
	cfg.WordList = []BannedWord{{Word: "work", Replacement: "play"}}
	store.setConfig(cfg)
	mustRepost(t, p, reposting("off to work, nigger"))
	_, posted := ops.snapshot()
	if got := strings.ToLower(posted[0].Content); strings.Contains(got, "nigger") || !strings.Contains(got, "play") {
		t.Errorf("posted %q", got)
	}
}

// A softened repost is the rewrite already: a verdict that would rewrite
// leaves it up, and still counts.
func TestASoftenedRepostStandsAsTheRewrite(t *testing.T) {
	p, store, ops := repostPlugin(t)
	p.scanning = false // the hint is tested on its own
	r := reposting("I'll come over tonight and boop you")
	r.Policy = string(BucketThreats)
	mustRepost(t, p, r)

	v := confirmed("r")
	v.Rewrite = "ik kom je vanavond opzoeken en dan aai ik je"
	for range 2 {
		p.enforce(context.Background(), enforcingConfig(), dutchMessage, BucketThreats, ActionRewrite, v)
	}
	deleted, posted := ops.snapshot()
	if !slices.Equal(deleted, []string{"m1"}) || len(posted) != 1 {
		t.Errorf("deleted %v posted %d, want only the repost's own swap", deleted, len(posted))
	}
	if inc := store.recorded(); len(inc) != 1 || inc[0].Action != ActionRewrite || inc[0].Content != dutch || inc[0].Replacement != "" {
		t.Errorf("incidents %+v", inc)
	}
	if len(ops.dms) != 1 {
		t.Errorf("%d dms, want the one that explains it", len(ops.dms))
	}

	// The copy is still answerable for its own words.
	own := dutchMessage
	own.MessageID, own.Content = "repost1", r.Text
	p.enforce(context.Background(), enforcingConfig(), own, BucketThreats, ActionRemove, confirmed("r"))
	if deleted, _ := ops.snapshot(); !slices.Contains(deleted, "repost1") {
		t.Errorf("deleted %v, want the repost", deleted)
	}
}

// A translation that was not softened carries the violation, so a verdict
// that would rewrite the original takes the copy down.
func TestAFaithfulRepostOfAViolationIsRemoved(t *testing.T) {
	p, store, ops := repostPlugin(t)
	mustRepost(t, p, reposting("I'm coming to hurt you"))
	v := confirmed("r")
	v.Rewrite = "ik kom je vanavond opzoeken en dan aai ik je"
	p.enforce(context.Background(), enforcingConfig(), dutchMessage, BucketThreats, ActionRewrite, v)
	if deleted, posted := ops.snapshot(); !slices.Equal(deleted, []string{"m1", "repost1"}) || len(posted) != 1 {
		t.Errorf("deleted %v posted %d", deleted, len(posted))
	}
	if inc := store.recorded(); len(inc) != 1 || inc[0].Action != ActionRemove {
		t.Errorf("incidents %+v", inc)
	}
}

// A kept original and its copy go together.
func TestAKeptOriginalGoesWithItsRepost(t *testing.T) {
	p, _, ops := repostPlugin(t)
	r := reposting("hello")
	r.Keep = true
	mustRepost(t, p, r)
	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Fatalf("deleted %v, want the original kept", deleted)
	}
	c := dutchMessage
	c.MessageID = "repost1"
	p.enforce(context.Background(), enforcingConfig(), c, BucketThreats, ActionRewrite, confirmed("r"))
	if deleted, _ := ops.snapshot(); !slices.Equal(deleted, []string{"m1", "repost1"}) {
		t.Errorf("deleted %v", deleted)
	}
}

// Softening a violation sends the original to the deep pass, so it costs its
// author what it would have.
func TestASoftenedOriginalIsJudged(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	ops.live = map[string]string{"m1": dutch}
	client := &fakeClassifier{deep: []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"r"}`}}
	p := completePlugin(t, store, enforcingConfig(), client)
	p.ops = func(string) DiscordOps { return ops }
	r := reposting("I'll boop you")
	r.Policy = string(BucketThreats)
	mustRepost(t, p, r)
	p.wg.Wait()
	if inc := store.recorded(); len(inc) != 1 || inc[0].Content != dutch {
		t.Errorf("incidents %+v, want the original's own words", inc)
	}

	// Child safety has no harmless version: nothing is reposted.
	r.MessageID, r.Policy = "m2", string(BucketChildSafety)
	ops.live["m2"] = dutch
	if _, err := p.Repost(context.Background(), r); !unavailable(err) {
		t.Errorf("err = %v", err)
	}
	r.Policy, r.Keep = string(BucketThreats), true
	if _, err := p.Repost(context.Background(), r); !unavailable(err) {
		t.Errorf("kept: err = %v", err)
	}
	if _, posted := ops.snapshot(); len(posted) != 1 {
		t.Errorf("posted %d", len(posted))
	}
	p.wg.Wait()
	if spent, limit, ok := p.Budget(context.Background(), "g1"); !ok || limit <= 0 || spent < 0 {
		t.Errorf("budget %v of %v, %v", spent, limit, ok)
	}
	if _, _, ok := p.WithGate(gateOff{}).Budget(context.Background(), "g1"); ok {
		t.Error("a disabled plugin has no budget to spend")
	}
}

func TestMovesAreForgotten(t *testing.T) {
	p, _, _ := repostPlugin(t)
	mustRepost(t, p, reposting("hello"))
	p.noteMove(&move{guildID: "g2", original: "a", repost: "b", at: testNow.Add(2 * moveTTL)})
	if p.moved("m1") != nil || p.moved("a") == nil {
		t.Error("an old move outlived moveTTL, or a new one did not survive")
	}
	p.ForgetGuild("g2")
	if p.moved("b") != nil {
		t.Error("a move outlived its guild")
	}
}

func TestClearingPendingFlagsFollowsTheRepost(t *testing.T) {
	p, store, ops := repostPlugin(t)
	mustRepost(t, p, reposting("hello"))
	_, _ = store.RecordIncident(context.Background(), Incident{GuildID: "g1", ChannelID: "c1", MessageID: "m1",
		AuthorID: "u1", Bucket: BucketThreats, Action: ActionFlag, CreatedAt: testNow})
	p.clearPendingFlags(context.Background(), enforcingConfig(), "u1")
	if deleted, _ := ops.snapshot(); !slices.Contains(deleted, "repost1") {
		t.Errorf("deleted %v", deleted)
	}
}

func TestPublishable(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &scriptedChat{}, newFakeOps(), &fakeAudit{})
	cfg := enforcingConfig()
	cfg.WordList = []BannedWord{{Word: "blorp", Replacement: "bloop"}}
	store.setConfig(cfg)
	for text, want := range map[string]bool{"Jamaican English": true, "dem late every time": true, "so blorp": false, "nigger": false} {
		if got := p.Publishable(context.Background(), "g1", text); got != want {
			t.Errorf("%q publishable %v", text, got)
		}
	}
}

func TestAskReservesAndFallsBack(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	chat := &scriptedChat{out: "{}"}
	p := completePlugin(t, store, cfg, chat)
	q := core.Prompt{System: "s", User: "u", SchemaName: "x", Schema: map[string]any{"type": "object"}, Reserve: 0.8}
	if out, err := p.Ask(context.Background(), "g1", q); err != nil || out.Text != "{}" {
		t.Fatalf("Ask = %+v, %v", out, err)
	}
	if chat.last.ResponseFormat == nil || chat.last.ResponseFormat.JSONSchema.Name != "x" {
		t.Errorf("request %+v carries no schema", chat.last)
	}
	// Past its share of the cap, with the rest still there for moderation.
	_ = store.AddSpend(context.Background(), "g1", today(testNow), Usage{Cost: cfg.DailyBudgetUSD * 0.85}, true)
	if _, err := p.Ask(context.Background(), "g1", q); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("err = %v", err)
	}

	// OrcaRouter failing falls over to OpenRouter once.
	store2 := newFakeStore()
	fake := &gatewayFails{spec: orcaRouter}
	p2 := completePlugin(t, store2, cfg, fake)
	live, _ := store2.Config(context.Background(), "g1")
	live.OrcaKeySealed = live.APIKeySealed
	store2.setConfig(live)
	if out, err := p2.Ask(context.Background(), "g1", q); err != nil || out.Text != "from openrouter" || fake.calls != 2 {
		t.Errorf("Ask = %+v, %v after %d calls", out, err, fake.calls)
	}
}

type gatewayFails struct {
	spec  *providerSpec
	calls int
}

func (g *gatewayFails) Chat(_ context.Context, _ string, req chatRequest) (string, Usage, error) {
	g.calls++
	if req.spec == g.spec {
		return "", Usage{}, errors.New("no answer")
	}
	return "from " + req.spec.name, Usage{Cost: 0.001}, nil
}

func TestSeamReads(t *testing.T) {
	p, store, ops := repostPlugin(t)
	cfg := enforcingConfig()
	cfg.MemberOptOut, cfg.OptOutUserIDs, cfg.EvidenceHours = true, []string{"u1"}, 6
	store.setConfig(cfg)
	ops.history = []Line{{AuthorID: "u2", Content: "hoi"}}
	ctx := context.Background()
	if !p.OptedOut(ctx, "g1", "u1") || p.OptedOut(ctx, "g1", "u2") || p.EvidenceHours(ctx, "g1") != 6 {
		t.Error("opt-out or evidence misread")
	}
	if lines, self := p.Context("g1", "c1", "m1", "u1"); self != "A" || !slices.Equal(lines, []string{"B: hoi"}) {
		t.Errorf("context %v self %q", lines, self)
	}
	if got := p.Policies(ctx, "g1"); !strings.Contains(got, "- threats: ") {
		t.Errorf("policies %q", got)
	}
	store.configErr = errors.New("down")
	if !p.OptedOut(ctx, "g1", "u2") || p.EvidenceHours(ctx, "g1") != 0 {
		t.Error("an unreadable config must read as opted out and keep nothing")
	}
}
