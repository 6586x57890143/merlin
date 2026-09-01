package aimod

import (
	"context"
	"errors"
	"github.com/6586x57890143/merlin/internal/secret"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The intake path, from a gateway event to whatever it costs. Everything
// here is about what does *not* happen: the free rungs deciding without
// spending, and the loop guard.

func intakePlugin(t *testing.T, store *fakeStore, client *fakeClassifier, ops *fakeOps) *Plugin {
	t.Helper()
	p := testPlugin(t, store, client, ops, &fakeAudit{})
	sealer, err := secret.New(testSecretKey)
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	p.sealer = sealer
	sealed, err := sealer.Seal("sk-or-v1-test")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	cfg := enforcingConfig()
	cfg.APIKeySealed = sealed
	store.setConfig(cfg)
	return p
}

// The loop guard, and the most expensive bug this package could have: a
// rewrite reposts through a webhook, that repost arrives here as a
// MessageCreate, and without this the plugin classifies its own output
// forever, at full price, until the budget runs out.
func TestRewriteRepostIsNotScanned(t *testing.T) {
	client := &fakeClassifier{}
	p := intakePlugin(t, newFakeStore(), client, newFakeOps())

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1", WebhookID: "w1",
		Content: "i think [removed] is wrong about this",
		Author:  &discordgo.User{ID: "u1", Username: "someguy"},
	})
	p.wg.Wait()

	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("classified this bot's own repost: fast=%d deep=%d", fast, deep)
	}
}

// A hard-hit pattern acts with no model in the loop, so it still works on a
// guild whose budget is spent or which never configured a key at all.
func TestHardHitActsWithoutAModel(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{}
	p := testPlugin(t, store, client, ops, &fakeAudit{})

	// No API key, and a budget of nothing.
	cfg := enforcingConfig()
	cfg.APIKeySealed = nil
	cfg.DailyBudgetUSD = 0
	store.setConfig(cfg)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "free nitro here https://grabify.link/abcdef",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	deleted, _ := ops.snapshot()
	if len(deleted) != 1 {
		t.Errorf("deleted %v, want the grabber link removed with no model involved", deleted)
	}
	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("called a model for a hard-hit pattern: fast=%d deep=%d", fast, deep)
	}
}

// A hard hit is never a rewrite: these patterns match a credential or a
// link, and a cleaned-up phishing message is still a phishing message with
// a hole in it.
func TestHardHitNeverRewrites(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	cfg := enforcingConfig()
	cfg.BucketActions[BucketMalicious] = ActionRewrite
	store.setConfig(cfg)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "grab it https://iplogger.org/abcdef quick",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	deleted, posted := ops.snapshot()
	if len(deleted) != 1 {
		t.Errorf("deleted %v, want the message removed", deleted)
	}
	if len(posted) != 0 {
		t.Errorf("reposted a cleaned version of a phishing message: %+v", posted)
	}
}

// A slur is the other half of that rule: the word is the violation, the
// sentence around it is not, so this one really is reposted cleaned up. It
// obeys hate_speech like any other hit in that bucket, off included.
func TestHardSlurIsRewrittenWhereHateSpeechIsOn(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	cfg := enforcingConfig()
	cfg.BucketActions[BucketHateSpeech] = ActionRewrite
	store.setConfig(cfg)

	msg := &discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "shut up you faggot",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	}
	p.HandleMessage(msg)
	p.wg.Wait()

	deleted, posted := ops.snapshot()
	if len(deleted) != 1 || len(posted) != 1 {
		t.Fatalf("deleted %v, posted %d, want the message replaced", deleted, len(posted))
	}
	if _, still := redactSlurs(posted[0].Content); still {
		t.Errorf("reposted %q, which still carries the word", posted[0].Content)
	}
	if !strings.HasPrefix(posted[0].Content, "shut up you ") {
		t.Errorf("reposted %q, which lost the rest of the sentence", posted[0].Content)
	}

	// The same message where the guild has hate_speech off is left alone:
	// rung 1 skips the model, it does not skip the policy.
	ops2, store2 := newFakeOps(), newFakeStore()
	p2 := testPlugin(t, store2, &fakeClassifier{}, ops2, &fakeAudit{})
	off := enforcingConfig()
	off.BucketActions[BucketHateSpeech] = ActionOff
	store2.setConfig(off)
	p2.HandleMessage(msg)
	p2.wg.Wait()
	if deleted, posted := ops2.snapshot(); len(deleted) != 0 || len(posted) != 0 {
		t.Errorf("acted with hate_speech off: deleted %v, posted %d", deleted, len(posted))
	}
}

// Nothing is read at all without the intent, and the plugin says so rather
// than appearing to work.
func TestNothingIsScannedWithoutTheIntent(t *testing.T) {
	client := &fakeClassifier{}
	ops := newFakeOps()
	p := intakePlugin(t, newFakeStore(), client, ops)
	p.scanning = false

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "free nitro here https://grabify.link/abcdef",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("acted on %v without the MESSAGE_CONTENT intent", deleted)
	}
}

// A DM has no guild, so no guild's policy applies to it and there is nothing
// to enforce against.
func TestDirectMessagesAreIgnored(t *testing.T) {
	client := &fakeClassifier{}
	p := intakePlugin(t, newFakeStore(), client, newFakeOps())

	p.HandleMessage(&discordgo.Message{
		ID: "m1", ChannelID: "c1",
		Content: "a long enough message with no guild attached",
		Author:  &discordgo.User{ID: "u1"},
	})
	p.wg.Wait()

	if fast, _ := client.counts(); fast != 0 {
		t.Errorf("classified a DM: %d fast calls", fast)
	}
}

// A full batch goes immediately rather than waiting out the window, so a
// busy channel gets both the cost saving and the lower latency.
func TestAFullBatchFlushesEarly(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{}
	p := intakePlugin(t, store, client, newFakeOps())

	for i := range batchMax {
		p.queue("g1", candidate{
			MessageID: string(rune('a' + i)), ChannelID: "c1", AuthorID: "u1",
			Content: "message number " + string(rune('a'+i)),
		})
	}
	p.wg.Wait()

	if fast, _ := client.counts(); fast != 1 {
		t.Errorf("made %d fast calls for one full batch, want 1", fast)
	}
	// One call carrying every message is the whole point: the policy prompt
	// is paid for once rather than twenty times.
	if n := strings.Count(client.lastFastReq.Messages[1].Content, "\n"); n < batchMax {
		t.Errorf("the batch carried %d lines, want %d messages in one call", n, batchMax)
	}
}

// A channel's queue is bounded, so a flood cannot make this plugin spend the
// rest of the day replaying a raid that was over minutes ago.
func TestQueueIsBounded(t *testing.T) {
	p := intakePlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps())

	// Stop the timer firing so the batch stays pending while it is filled.
	p.batchMu.Lock()
	p.batches["c1"] = &batch{guildID: "g1", timer: time.NewTimer(time.Hour)}
	p.batchMu.Unlock()

	for i := range queueMax + 50 {
		p.queue("g1", candidate{MessageID: "m", ChannelID: "c1", AuthorID: "u1", Content: string(rune(i))})
	}

	p.batchMu.Lock()
	defer p.batchMu.Unlock()
	if b := p.batches["c1"]; b != nil && len(b.candidates) > queueMax {
		t.Errorf("queued %d messages, past the bound of %d", len(b.candidates), queueMax)
	}
}

// Shutdown waits for work already in flight, so a restart does not delete a
// message on the way out and lose the incident row that explains it.
func TestShutdownWaitsForInFlightWork(t *testing.T) {
	p := intakePlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps())

	started := make(chan struct{})
	release := make(chan struct{})
	p.spawn(func(context.Context) {
		close(started)
		<-release
	})
	<-started

	done := make(chan error, 1)
	go func() { done <- p.Shutdown(context.Background()) }()

	select {
	case <-done:
		t.Fatal("Shutdown returned while work was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-done; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// fakeGate reports a fixed answer, standing in for
// internal/settings.Store's per-guild plugin toggle.
type fakeGate struct{ enabled bool }

func (f fakeGate) PluginEnabled(string, string) bool { return f.enabled }

// Disabling the plugin has to stop the scanning, not just the commands.
// HandleMessage is the only entry point in this codebase that the command
// router's gate check never sees, and getting this wrong left an admin who
// had just disabled the plugin watching it keep deleting messages with no
// command left to stop it.
func TestDisabledPluginScansNothing(t *testing.T) {
	ops := newFakeOps()
	client := &fakeClassifier{}
	p := intakePlugin(t, newFakeStore(), client, ops)
	p.gate = fakeGate{enabled: false}

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "free nitro here https://grabify.link/abcdef",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("a disabled plugin deleted %v", deleted)
	}
	if fast, _ := client.counts(); fast != 0 {
		t.Errorf("a disabled plugin made %d model calls", fast)
	}
}

// An enabled guild is unaffected, and so is a build with no gate wired in
// (which is what the other tests run as).
func TestEnabledPluginStillScans(t *testing.T) {
	ops := newFakeOps()
	p := intakePlugin(t, newFakeStore(), &fakeClassifier{}, ops)
	p.gate = fakeGate{enabled: true}

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "free nitro here https://grabify.link/abcdef",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v, want the grabber link removed", deleted)
	}
}

// The deep pass gets five preceding lines, and until they carried a speaker
// they were an undifferentiated blob: two people trading insults and one
// person escalating at a third read identically. Labels are what make the
// context usable, and the candidate's own author has to be findable among
// them or the model cannot tell whether the flagged message is a reply.
func TestRecentContextLabelsSpeakers(t *testing.T) {
	ops := newFakeOps()
	// ChannelMessages returns newest-first, so this is reversed on the way
	// out: u2, u1, u2 in the order they were said.
	ops.history = []*discordgo.Message{
		{Content: "third", Author: &discordgo.User{ID: "u2"}},
		{Content: "second", Author: &discordgo.User{ID: "u1"}},
		{Content: "first", Author: &discordgo.User{ID: "u2"}},
	}
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	lines, self := p.recentContext("g1", candidate{ChannelID: "c1", AuthorID: "u1"})

	if self != "A" {
		t.Errorf("self = %q, want A: the judged author is labelled first", self)
	}
	want := []string{"B: first", "A: second", "B: third"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			// One label per author, stable across lines: "B" twice for u2 is
			// the whole signal, and a fresh letter per message would destroy
			// it while looking like it worked.
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// Without this the whole plugin has a one-step bypass: post something
// innocuous, let it be cleared, then edit it into whatever you like.
func TestEditedMessagesAreScanned(t *testing.T) {
	ops := newFakeOps()
	p := intakePlugin(t, newFakeStore(), &fakeClassifier{}, ops)

	edited := time.Now()
	p.HandleMessageEdit(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content:         "free nitro here https://grabify.link/abcdef",
		Author:          &discordgo.User{ID: "u1"},
		Member:          &discordgo.Member{},
		EditedTimestamp: &edited,
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v, want the edited-in grabber link removed", deleted)
	}
}

// MessageUpdate also fires when Discord resolves a link preview or pins a
// message, with the content untouched. Scanning those would raise traffic
// substantially and decide nothing.
func TestUneditedUpdatesAreIgnored(t *testing.T) {
	ops := newFakeOps()
	p := intakePlugin(t, newFakeStore(), &fakeClassifier{}, ops)

	p.HandleMessageEdit(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "free nitro here https://grabify.link/abcdef",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("acted on %v for an update that changed nothing", deleted)
	}
}

// The fast pass is the gate, and it sees each message alone: what it clears,
// the deep pass never sees. A reply target is the one piece of context that
// is free (Discord ships it on the gateway payload) and decisive, since "kys"
// is innocuous standalone and the whole violation aimed at somebody.
func TestReplyTargetReachesTheFastPass(t *testing.T) {
	client := &fakeClassifier{fast: []string{`{"v":[]}`}}
	p := intakePlugin(t, newFakeStore(), client, newFakeOps())

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "you should genuinely do it",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
		ReferencedMessage: &discordgo.Message{
			Content: "i have been thinking about hurting myself",
			Author:  &discordgo.User{ID: "u2"},
		},
	})
	p.flush("c1")
	p.wg.Wait()

	if got := client.lastUserMessage(); !strings.Contains(got, "replying to:") ||
		!strings.Contains(got, "thinking about hurting myself") {
		t.Errorf("the reply target never reached the fast prompt:\n%s", got)
	}
}

// Forwarding was a one-click bypass of the whole plugin: Discord puts the
// original text in a snapshot and leaves Content empty, so every rung keyed
// off Content saw nothing and shouldSkip returned skipEmpty.
func TestForwardedMessagesAreScanned(t *testing.T) {
	ops := newFakeOps()
	p := intakePlugin(t, newFakeStore(), &fakeClassifier{}, ops)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
		MessageSnapshots: []discordgo.MessageSnapshot{
			{Message: &discordgo.Message{Content: "free nitro here https://grabify.link/abcdef"}},
		},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v, want the forwarded grabber link removed", deleted)
	}
}

// A forward can carry several snapshots, so stopping at the first would leave
// one whose opening line is innocuous and whose second is not doing exactly
// what this closes.
func TestMessageTextJoinsEverySnapshot(t *testing.T) {
	text, forwarded := messageText(&discordgo.Message{
		MessageSnapshots: []discordgo.MessageSnapshot{
			{Message: &discordgo.Message{Content: "look at this"}},
			{Message: &discordgo.Message{Content: "the actual violation"}},
		},
	})
	if !forwarded {
		t.Error("a message whose only text was in a snapshot did not read as forwarded")
	}
	if !strings.Contains(text, "look at this") || !strings.Contains(text, "the actual violation") {
		t.Errorf("messageText = %q, want both snapshots", text)
	}
}

// The member's own typing wins over a snapshot, and an ordinary message must
// not read as forwarded: that flag downgrades rewrite to remove.
func TestMessageTextPrefersTheMembersOwnContent(t *testing.T) {
	text, forwarded := messageText(&discordgo.Message{
		Content: "what i actually typed",
		MessageSnapshots: []discordgo.MessageSnapshot{
			{Message: &discordgo.Message{Content: "something forwarded alongside it"}},
		},
	})
	if forwarded || text != "what i actually typed" {
		t.Errorf("messageText = (%q, %v), want the member's own text and false", text, forwarded)
	}
}

// A batch with enough hits used to lose its tail: escalations ran serially at
// up to 20s each inside a 2 minute spawn, so six flagged messages was exactly
// the budget and the rest were cancelled with no incident row at all.
func TestEveryHitInALargeBatchIsActedOn(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{
		fast: []string{`{"v":[
			{"i":1,"b":"threats","c":0.9},{"i":2,"b":"threats","c":0.9},
			{"i":3,"b":"threats","c":0.9},{"i":4,"b":"threats","c":0.9},
			{"i":5,"b":"threats","c":0.9},{"i":6,"b":"threats","c":0.9},
			{"i":7,"b":"threats","c":0.9},{"i":8,"b":"threats","c":0.9}
		]}`},
		deep:      []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"r"}`},
		deepDelay: 50 * time.Millisecond,
	}
	p := intakePlugin(t, store, client, ops)

	batch := make([]candidate, 8)
	for i := range batch {
		batch[i] = candidate{
			MessageID: "m" + strconv.Itoa(i), ChannelID: "c1",
			// Distinct authors: the per-member deep ceiling is not what this
			// test is about, and eight from one member would trip it.
			AuthorID: "u" + strconv.Itoa(i),
			Content:  "message number " + strconv.Itoa(i),
		}
	}
	p.classify("g1", batch)
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 8 {
		t.Errorf("deleted %d of 8 hits: the batch lost its tail", len(deleted))
	}
	if got := len(store.recorded()); got != 8 {
		t.Errorf("recorded %d incidents of 8", got)
	}
	// The count alone would pass serially too, since eight 50ms calls fit
	// inside a test's patience even though eight 20s ones do not fit inside
	// spawn's two minutes. This is the assertion that fails the moment
	// escalation goes back to being serial.
	if peak := client.peakDeepParallel(); peak < 2 {
		t.Errorf("peak concurrent deep calls = %d: escalations ran serially", peak)
	}
	// And still bounded, so a batch of twenty hits cannot fire twenty
	// simultaneous requests on one guild's key.
	if peak := client.peakDeepParallel(); peak > maxConcurrentEscalations {
		t.Errorf("peak concurrent deep calls = %d, past the bound of %d", peak, maxConcurrentEscalations)
	}
}

// A message that replies to nothing must not carry the marker, or every
// batch pays for a clause that says nothing.
func TestNonReplyCarriesNoReplyMarker(t *testing.T) {
	if got := replyContext(&discordgo.Message{Content: "hello"}); got != "" {
		t.Errorf("replyContext = %q for a message replying to nothing", got)
	}
}

// An unreadable history is not a reason to refuse to judge, but the label
// still has to come back or classifyDeep names an empty speaker.
func TestRecentContextSelfLabelSurvivesAFailedFetch(t *testing.T) {
	ops := newFakeOps()
	ops.historyErr = errors.New("missing read history permission")
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	lines, self := p.recentContext("g1", candidate{ChannelID: "c1", AuthorID: "u1"})
	if lines != nil {
		t.Errorf("lines = %v, want none", lines)
	}
	if self != "A" {
		t.Errorf("self = %q, want A", self)
	}
}
