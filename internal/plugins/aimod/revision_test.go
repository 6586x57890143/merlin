package aimod

import (
	"context"
	"testing"
)

// midFlight runs during the deep call, the second call classifyOne makes,
// which is where a slow model leaves time for the world to change.
type midFlight struct {
	*fakeClassifier
	calls  int
	during func()
}

func (m *midFlight) Chat(ctx context.Context, key string, req chatRequest) (string, Usage, error) {
	if m.calls++; m.calls == 2 {
		m.during()
	}
	return m.fakeClassifier.Chat(ctx, key, req)
}

func confirmingClient() *fakeClassifier {
	return &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.8}]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"r"}`},
	}
}

var threat = candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "some genuinely threatening sentence"}

// Switching the plugin off while a verdict is in flight must stop that
// verdict, not only the next one.
func TestTurningModerationOffStopsAVerdictInFlight(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	cfg := enforcingConfig()
	store.setConfig(cfg)
	p := testPlugin(t, store, &midFlight{fakeClassifier: confirmingClient(), during: func() {
		off := cfg
		off.Mode = ModeOff
		store.setConfig(off)
	}}, ops, &fakeAudit{})

	p.classifyOne(t, cfg, threat)

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v after the guild switched moderation off", deleted)
	}
	if len(store.recorded()) != 0 || len(ops.timeouts) != 0 {
		t.Error("a verdict reached before the switch-off was still recorded or sanctioned")
	}
}

// A bucket moved to flag meanwhile is flagged, not acted on.
func TestABucketMovedToFlagInFlightIsOnlyFlagged(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	cfg := enforcingConfig()
	store.setConfig(cfg)
	p := testPlugin(t, store, &midFlight{fakeClassifier: confirmingClient(), during: func() {
		flag := enforcingConfig()
		flag.BucketActions[BucketThreats] = ActionFlag
		store.setConfig(flag)
	}}, ops, &fakeAudit{})

	p.classifyOne(t, cfg, threat)

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v from a bucket set to flag", deleted)
	}
	if inc := store.recorded(); len(inc) != 1 || inc[0].Action != ActionFlag {
		t.Errorf("recorded %+v, want one flag", inc)
	}
}

// An author who edits while the verdict is in flight is judged on what
// they now say, by the rescan the edit triggers, not on what they said.
func TestAVerdictOnEditedTextIsDiscarded(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	cfg := enforcingConfig()
	store.setConfig(cfg)
	mf := &midFlight{fakeClassifier: confirmingClient()}
	p := testPlugin(t, store, mf, ops, &fakeAudit{})
	// What HandleMessageEdit records before it rescans.
	mf.during = func() { p.noteRevision("m1", "sorry, that came out wrong") }

	p.classifyOne(t, cfg, threat)

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v on the strength of text the author had already edited away", deleted)
	}
	if len(store.recorded()) != 0 {
		t.Error("an incident was recorded for words no longer in the channel")
	}

	// The unedited path still acts.
	p.noteRevision("m2", threat.Content)
	c := threat
	c.MessageID = "m2"
	mf.calls, mf.during = 0, func() {}
	p.classifyOne(t, cfg, c)
	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v, want the message whose text still matches", deleted)
	}
}
