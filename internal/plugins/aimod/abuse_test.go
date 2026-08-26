package aimod

import (
	"context"
	"testing"
	"time"
)

// The meters are what stop one member spending a server's whole daily
// budget. The damage that does is not the bill (the budget caps that) but
// the hours of unprotected server afterwards, which is exactly what somebody
// planning to post something reportable would want to arrange first.

func TestScanCeilingStopsOneMemberEatingTheBudget(t *testing.T) {
	m := newUserMeter()

	for i := range maxUserScans {
		if !m.allowScan("g1", "u1", testNow) {
			t.Fatalf("refused scan %d of an allowed %d", i+1, maxUserScans)
		}
	}
	if m.allowScan("g1", "u1", testNow) {
		t.Error("allowed a scan past the ceiling")
	}

	// Another member is unaffected: the ceiling is per person, not a shared
	// pool one loud member can drain for everybody.
	if !m.allowScan("g1", "u2", testNow) {
		t.Error("one member hitting their ceiling blocked a different member")
	}
	// And so is the same member in a different guild.
	if !m.allowScan("g2", "u1", testNow) {
		t.Error("a ceiling in one guild blocked the same member in another")
	}
}

// A sliding window, not a counter with a reset: otherwise a member stays
// quiet until the boundary and then bursts straight through it.
func TestScanCeilingSlides(t *testing.T) {
	m := newUserMeter()
	for range maxUserScans {
		m.allowScan("g1", "u1", testNow)
	}
	if m.allowScan("g1", "u1", testNow.Add(meterWindow-time.Second)) {
		t.Error("the window expired early")
	}
	if !m.allowScan("g1", "u1", testNow.Add(meterWindow+time.Second)) {
		t.Error("the window never expired, so the member is blocked forever")
	}
}

// The deep ceiling is the tighter one, because the deep pass costs two
// orders of magnitude more and is the rung somebody who has worked out a
// trigger phrase can drive on demand.
func TestDeepCeilingFiresItsCrossingExactlyOnce(t *testing.T) {
	m := newUserMeter()

	for i := range maxUserDeep {
		allowed, crossed := m.allowDeep("g1", "u1", testNow)
		if !allowed {
			t.Fatalf("refused deep escalation %d of an allowed %d", i+1, maxUserDeep)
		}
		if crossed {
			t.Fatalf("reported a crossing on escalation %d, before the ceiling", i+1)
		}
	}

	allowed, crossed := m.allowDeep("g1", "u1", testNow)
	if allowed {
		t.Error("allowed a deep escalation past the ceiling")
	}
	if !crossed {
		t.Error("the crossing was not reported, so no audit entry and no sanction fire")
	}

	// Every attempt after it is still refused, and reports no crossing, so
	// the audit channel gets one line rather than one per message.
	for range 5 {
		allowed, crossed := m.allowDeep("g1", "u1", testNow)
		if allowed || crossed {
			t.Errorf("after the crossing: allowed=%v crossed=%v, want false and false", allowed, crossed)
		}
	}
}

func TestDeepCeilingIsTighterThanTheScanCeiling(t *testing.T) {
	// Not arbitrary trivia: if these ever invert, the expensive rung becomes
	// the one with the loose limit.
	if maxUserDeep >= maxUserScans {
		t.Errorf("maxUserDeep (%d) is not below maxUserScans (%d)", maxUserDeep, maxUserScans)
	}
}

// Crossing the ceiling is reported even in a guild that has asked the bot
// not to act on it. A member generating that much flagged content is
// something a moderator should know about, and it is the only warning an
// admin gets that their budget is being drained deliberately.
func TestAbuseIsAlwaysAudited(t *testing.T) {
	for _, action := range []SanctionAction{SanctionOff, SanctionFlag} {
		t.Run(string(action), func(t *testing.T) {
			audit := &fakeAudit{}
			jailer := &fakeJailer{}
			p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), audit)
			p.jailer = jailer

			cfg := enforcingConfig()
			cfg.SanctionAction = action
			p.handleAbuse(context.Background(), cfg, candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"})

			if !contains(audit.actions(), "aimod.abuse_detected") {
				t.Errorf("audit actions = %v, want aimod.abuse_detected", audit.actions())
			}
			if len(jailer.jailed()) != 0 {
				t.Error("jailed somebody in a guild that did not ask for automatic jails")
			}
		})
	}
}

func TestAbuseJailsWhenAsked(t *testing.T) {
	audit := &fakeAudit{}
	jailer := &fakeJailer{}
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), audit)
	p.jailer = jailer

	cfg := sanctioningConfig()
	p.handleAbuse(context.Background(), cfg, candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"})

	jailed := jailer.jailed()
	if len(jailed) != 1 {
		t.Fatalf("jailed %d members, want 1", len(jailed))
	}
	if jailed[0].duration != abuseBase {
		t.Errorf("duration = %s, want the %s abuse base for a first offence", jailed[0].duration, abuseBase)
	}
	if !contains(audit.actions(), "aimod.sanctioned") {
		t.Errorf("audit actions = %v, want aimod.sanctioned", audit.actions())
	}
}

// A member over their deep ceiling is still recorded as flagged. Dropping
// them silently would let somebody bury a real violation inside a flood,
// which is the exact move the ceiling would otherwise enable.
func TestOverTheDeepCeilingStillRecordsAFlag(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.99,"reason":"r"}`},
	}
	p := testPlugin(t, store, client, ops, &fakeAudit{})
	cfg := enforcingConfig()

	// Burn the deep ceiling, then send one more.
	for range maxUserDeep + 2 {
		p.escalate(context.Background(), cfg, "key",
			candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "x"},
			Verdict{Index: 1, Bucket: BucketThreats, Confidence: 0.9})
	}

	_, deep := client.counts()
	if deep > maxUserDeep {
		t.Errorf("made %d deep calls, past the ceiling of %d", deep, maxUserDeep)
	}
	var flags int
	for _, inc := range store.recorded() {
		if inc.Action == ActionFlag {
			flags++
		}
	}
	if flags == 0 {
		t.Error("a member over their ceiling had their messages dropped with no record at all")
	}
}

// Leaving a guild drops its meters, so a re-add does not start somebody
// mid-window against a membership that has since ended.
func TestForgetGuildClearsMeters(t *testing.T) {
	m := newUserMeter()
	for range maxUserScans {
		m.allowScan("g1", "u1", testNow)
	}
	m.allowScan("g2", "u1", testNow)

	m.forgetGuild("g1")
	if !m.allowScan("g1", "u1", testNow) {
		t.Error("the guild's meters survived the bot leaving it")
	}
	// The other guild is untouched.
	if _, ok := m.entries[meterKey{guildID: "g2", userID: "u1"}]; !ok {
		t.Error("forgetting one guild dropped another guild's meters")
	}
}

// The exploit the scan ceiling created, and the reason clearing exists: past
// the ceiling this plugin stops paying to confirm anything, so a member's
// messages carry on being flagged and stop being removed. Five deliberate
// trips bought ten minutes of saying whatever you liked.
func TestSanctionClearsWhatTheCeilingLetThrough(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	jailer := &fakeJailer{}
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
	p.jailer = jailer

	cfg := sanctioningConfig()
	cfg.BucketActions[BucketThreats] = ActionRemove
	cfg.BucketActions[BucketGore] = ActionFlag

	// Three messages flagged while the member was over their ceiling: two on
	// a bucket this guild removes, one on a bucket it only watches.
	for _, tc := range []struct {
		id     string
		bucket Bucket
	}{{"m1", BucketThreats}, {"m2", BucketThreats}, {"m3", BucketGore}} {
		if _, err := store.RecordIncident(context.Background(), Incident{
			GuildID: "g1", ChannelID: "c1", MessageID: tc.id, AuthorID: "u1",
			Bucket: tc.bucket, Action: ActionFlag, CreatedAt: testNow,
		}); err != nil {
			t.Fatalf("RecordIncident: %v", err)
		}
	}

	p.handleAbuse(context.Background(), cfg, candidate{MessageID: "m4", ChannelID: "c1", AuthorID: "u1"})

	deleted, _ := ops.snapshot()
	if len(deleted) != 2 {
		t.Errorf("deleted %v, want the two on a bucket this guild removes", deleted)
	}
	for _, id := range deleted {
		if id == "m3" {
			t.Error("deleted a message on a bucket the guild set to flag: a ceiling trip is not a reason to enforce a rule they switched off")
		}
	}
	if len(jailer.jailed()) != 1 {
		t.Error("the member was not jailed, so the next message is unaffected")
	}
	// The rows stop being flags, so a second trip does not delete them twice.
	var stillFlagged int
	for _, inc := range store.recorded() {
		if inc.MessageID != "m4:sanction" && inc.Action == ActionFlag && inc.MessageID != "m3" {
			stillFlagged++
		}
	}
	if stillFlagged != 0 {
		t.Errorf("%d cleared messages are still recorded as flags", stillFlagged)
	}
}

// A moderator who restored something has overruled the filter, and a later
// ceiling trip must not quietly undo that.
func TestClearingSkipsWhatAModeratorReversed(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
	p.jailer = &fakeJailer{}

	cfg := sanctioningConfig()
	cfg.BucketActions[BucketThreats] = ActionRemove
	id, err := store.RecordIncident(context.Background(), Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionFlag, CreatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	if err := store.MarkUndone(context.Background(), id); err != nil {
		t.Fatalf("MarkUndone: %v", err)
	}

	p.handleAbuse(context.Background(), cfg, candidate{MessageID: "m2", ChannelID: "c1", AuthorID: "u1"})

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v, which a moderator had already restored", deleted)
	}
}
