package aimod

import (
	"context"
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
