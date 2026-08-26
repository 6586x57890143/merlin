package aimod

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The free rungs decide the overwhelming majority of messages, so a bug here
// is either a cost regression measured in dollars a day or a silent hole in
// the filter, and neither announces itself.

func msg(content string) *discordgo.Message {
	return &discordgo.Message{
		ID:        "m1",
		GuildID:   "g1",
		ChannelID: "c1",
		Content:   content,
		Author:    &discordgo.User{ID: "u1"},
		Member:    &discordgo.Member{Roles: []string{"r-member"}},
	}
}

func TestShouldSkip(t *testing.T) {
	enforcing := Config{GuildID: "g1", Mode: ModeEnforce}

	tests := []struct {
		name string
		cfg  Config
		msg  func(*discordgo.Message)
		want skipReason
	}{
		{"ordinary message is scanned", enforcing, nil, skipNone},
		{
			"mode off skips everything",
			Config{GuildID: "g1", Mode: ModeOff},
			nil,
			skipMode,
		},
		{
			// Also the loop guard: a rewrite reposts through a webhook and
			// that repost arrives here as a MessageCreate. Without this the
			// plugin classifies its own output, forever, at full price.
			"webhook message is skipped",
			enforcing,
			func(m *discordgo.Message) { m.WebhookID = "w1" },
			skipWebhook,
		},
		{
			"bot author is skipped",
			enforcing,
			func(m *discordgo.Message) { m.Author.Bot = true },
			skipBot,
		},
		{
			"exempt channel is skipped",
			Config{GuildID: "g1", Mode: ModeEnforce, ExemptChannelIDs: []string{"c1"}},
			nil,
			skipChannel,
		},
		{
			"exempt role is skipped",
			Config{GuildID: "g1", Mode: ModeEnforce, ExemptRoleIDs: []string{"r-member"}},
			nil,
			skipRole,
		},
		{
			"attachment-only message is skipped",
			enforcing,
			func(m *discordgo.Message) { m.Content = "   " },
			skipEmpty,
		},
		{
			"short message is skipped",
			enforcing,
			func(m *discordgo.Message) { m.Content = "lol ok" },
			skipShort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{dedupe: newDedupeCache()}
			m := msg("this is a long enough message to be scanned")
			if tc.msg != nil {
				tc.msg(m)
			}
			if got := p.shouldSkip(tc.cfg, m, testNow); got != tc.want {
				t.Errorf("shouldSkip = %q, want %q", got, tc.want)
			}
		})
	}
}

// Copypasta, a member repeating themselves, and fifty raid accounts posting
// one line all collapse to a single model call. This is the cheapest of the
// cost levers and the easiest to break by moving the check.
func TestCleanDuplicateTextIsScannedOnce(t *testing.T) {
	p := &Plugin{dedupe: newDedupeCache()}
	cfg := Config{GuildID: "g1", Mode: ModeEnforce}
	const line = "join my server for free nitro now"

	if got := p.shouldSkip(cfg, msg(line), testNow); got != skipNone {
		t.Fatalf("first sighting = %q, want it scanned", got)
	}
	// Nothing is deduped until a scan has actually cleared it.
	if got := p.shouldSkip(cfg, msg(line), testNow); got != skipNone {
		t.Fatalf("second sighting before any verdict = %q, want it scanned too", got)
	}
	p.dedupe.markClean("g1", line, testNow)

	if got := p.shouldSkip(cfg, msg(line), testNow.Add(time.Minute)); got != skipDuplicate {
		t.Errorf("repeat of cleared text = %q, want skipDuplicate", got)
	}
	// Case and surrounding whitespace are not a way around it.
	if got := p.shouldSkip(cfg, msg("  JOIN MY SERVER FOR FREE NITRO NOW  "), testNow.Add(2*time.Minute)); got != skipDuplicate {
		t.Errorf("case variant = %q, want skipDuplicate", got)
	}
	// Past the window it is a fresh message again: the same words genuinely
	// can mean something different in a different conversation.
	if got := p.shouldSkip(cfg, msg(line), testNow.Add(dedupeWindow+time.Second)); got != skipNone {
		t.Errorf("after the window = %q, want it scanned again", got)
	}
}

// The behaviour two earlier designs got wrong in opposite directions, and
// the reason the cache holds a verdict rather than a sighting.
//
// The first recorded every message as seen and skipped repeats outright, so
// somebody who posted the same slur four times had one flagged and three
// silently dropped. The second only remembered clean text, which fixed that
// but made every repeat pay full price, so a flood could exhaust a scan
// ceiling with copies of one line. Both copies must be acted on, and only
// the first may cost anything.
func TestRepeatedViolationIsActedOnWithoutPayingAgain(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"a specific threat"}`},
	}
	p := intakePlugin(t, store, client, ops)

	const line = "a message that trips the filter every single time"
	send := func(id string) {
		p.HandleMessage(&discordgo.Message{
			ID: id, GuildID: "g1", ChannelID: "c1", Content: line,
			Author: &discordgo.User{ID: "u1"}, Member: &discordgo.Member{},
		})
		p.flush("c1")
		p.wg.Wait()
	}

	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		send(id)
	}

	// One classification, four removals.
	fast, deep := client.counts()
	if fast != 1 || deep != 1 {
		t.Errorf("calls: fast=%d deep=%d, want 1 and 1 for four copies of one message", fast, deep)
	}
	deleted, _ := ops.snapshot()
	if len(deleted) != 4 {
		t.Errorf("deleted %v, want all four copies acted on", deleted)
	}
	// And the repeats never touched the member's ceiling, which stays for
	// content nobody has judged yet.
	if _, crossed := p.meter.allowDeep("g1", "u1", testNow); crossed {
		t.Error("repeats consumed the deep ceiling despite costing nothing")
	}
}

// A message the deep pass cleared is genuinely clean, so later copies must
// not pay to be told so a second time.
func TestClearedTextIsRememberedAsClean(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
		deep: []string{`{"violation":false,"bucket":"threats","confidence":0.1,"reason":"hyperbole"}`},
	}
	p := intakePlugin(t, store, client, newFakeOps())

	const line = "i will end you at mario kart later tonight"
	p.classify("g1", []candidate{{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: line}})
	p.wg.Wait()

	if !p.dedupe.seenClean("g1", line, testNow) {
		t.Error("text the deep pass cleared was not remembered, so every copy pays again")
	}
}

// A guild that switches a policy area off after a verdict was cached must
// stop acting on it, so the action is re-read from live config rather than
// taken from the cache.
func TestCachedVerdictHonoursACurrentPolicyChange(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	p := intakePlugin(t, store, &fakeClassifier{}, ops)

	const line = "a message somebody already had a verdict on"
	p.dedupe.remember("g1", line, testNow, &cachedVerdict{
		bucket: BucketGore, action: ActionRemove,
		deep: deepVerdict{Violation: true, Bucket: BucketGore, Confidence: 0.99, Reason: "r"},
	})

	cfg, err := store.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	cfg.BucketActions[BucketGore] = ActionOff
	store.setConfig(cfg)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1", Content: line,
		Author: &discordgo.User{ID: "u1"}, Member: &discordgo.Member{},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v on a bucket the guild has since switched off", deleted)
	}
}

// One guild's traffic must not decide another's, since a verdict is only
// meaningful against one guild's own policy.
func TestDedupeIsPerGuild(t *testing.T) {
	c := newDedupeCache()
	c.markClean("g1", "hello there everyone", testNow)
	if !c.seenClean("g1", "hello there everyone", testNow) {
		t.Fatal("a cleared text was not remembered for its own guild")
	}
	if c.seenClean("g2", "hello there everyone", testNow) {
		t.Error("a different guild's identical text was treated as already cleared")
	}
}

func TestHardHit(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Bucket
		hit     bool
	}{
		{
			"ip grabber link",
			"click here lol https://grabify.link/abc123",
			BucketMalicious, true,
		},
		{
			"phishing domain",
			"free nitro at https://discord-gift.ru/claim",
			BucketMalicious, true,
		},
		{
			"ssn",
			"his ssn is 123-45-6789 go get him",
			BucketDoxxing, true,
		},
		{
			// The ranges the US never issues are what somebody reaches for
			// when writing a fake one to make a point.
			"placeholder ssn is not a hit",
			"an SSN looks like 000-00-0000, never share yours",
			"", false,
		},
		{
			"ordinary discord link",
			"come join https://discord.gg/abcdef we are nice",
			"", false,
		},
		{
			// The single most important negative case in this file. Regex is
			// a spam gate; anything needing judgement belongs to the model
			// rungs, where a wrong answer at least read the sentence.
			"rude message is not a hard hit",
			"you are an absolute clown and everyone here knows it",
			"", false,
		},
		{
			"a date is not an ssn",
			"the release was 2024-01-15 and the patch 2024-02-20",
			"", false,
		},
		{
			"phone number is not an ssn",
			"call the office on 555-0100 during the week",
			"", false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bucket, reason, hit := hardHit(tc.content)
			if hit != tc.hit {
				t.Fatalf("hardHit = %v, want %v (bucket %q, reason %q)", hit, tc.hit, bucket, reason)
			}
			if hit && bucket != tc.want {
				t.Errorf("bucket = %q, want %q", bucket, tc.want)
			}
			if hit && strings.TrimSpace(reason) == "" {
				t.Error("a hit with no reason, which is what a mod reads in the audit log")
			}
		})
	}
}

func TestEnforcedBuckets(t *testing.T) {
	// Only what a guild actually enforces is sent to the fast pass, which is
	// a policy decision and a token saving at once.
	cfg := Config{BucketActions: map[Bucket]Action{
		BucketThreats:    ActionOff,
		BucketHateSpeech: ActionFlag,
	}}
	got := enforcedBuckets(cfg)

	has := func(b Bucket) bool {
		for _, v := range got {
			if v == b {
				return true
			}
		}
		return false
	}
	if has(BucketThreats) {
		t.Error("a bucket switched off is still being sent to the model")
	}
	if !has(BucketHateSpeech) {
		t.Error("a bucket set to flag is not being sent to the model, so nothing will ever flag")
	}
	if !has(BucketChildSafety) {
		t.Error("child safety is missing, and it is the one bucket that cannot be off")
	}
}
