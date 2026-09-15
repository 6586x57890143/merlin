package aimod

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Screen is what /whisper asks before posting in a member's name. The
// failure directions are the point of every case here: a hard hit refuses
// with no model, an unfunded or switched-off filter says nothing rather than
// refusing, and a confirmed verdict refuses whatever the guild's per-bucket
// action is.

func TestScreenRefusesAHardSlurWithoutAModel(t *testing.T) {
	client := &fakeClassifier{}
	p := intakePlugin(t, newFakeStore(), client, newFakeOps())

	refusal, err := p.Screen(context.Background(), "g1", "c1", "u1", "you absolute n.i.g.g.e.r")
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if refusal == "" {
		t.Fatal("a hard slur was cleared for posting")
	}
	if strings.Contains(refusal, "ninja") {
		t.Errorf("refusal %q carries the rewrite: a whisper is refused, never substituted", refusal)
	}
	if fast, deep := client.counts(); fast+deep != 0 {
		t.Errorf("model called %d times for a rung 1 hit", fast+deep)
	}
}

// Rung 1 owes nothing to the switch: a slur is refused on a guild where aimod
// is off, disabled or unfunded, because merlin is the one posting it.
func TestScreenRunsRungOneWithAimodOff(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{}
	p := intakePlugin(t, store, client, newFakeOps())
	cfg := enforcingConfig()
	cfg.Mode = ModeOff
	store.setConfig(cfg)

	if refusal, _ := p.Screen(context.Background(), "g1", "c1", "u1", "f*ggot"); refusal == "" {
		t.Error("slur cleared because the model rungs are off")
	}
	if refusal, err := p.Screen(context.Background(), "g1", "c1", "u1", "an ordinary sentence"); refusal != "" || err != nil {
		t.Errorf("ordinary text with aimod off: refusal=%q err=%v, want clean and nil", refusal, err)
	}
	if fast, deep := client.counts(); fast+deep != 0 {
		t.Errorf("model called %d times with aimod off", fast+deep)
	}
}

func TestScreenPostsOnRegexAloneWithNoKey(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{}
	p := intakePlugin(t, store, client, newFakeOps())
	cfg := enforcingConfig()
	cfg.APIKeySealed = nil
	store.setConfig(cfg)

	refusal, err := p.Screen(context.Background(), "g1", "c1", "u1", "an ordinary sentence")
	if refusal != "" || err != nil {
		t.Errorf("unfunded guild: refusal=%q err=%v, want clean and nil", refusal, err)
	}
	if fast, _ := client.counts(); fast != 0 {
		t.Errorf("fast pass called %d times with no key", fast)
	}
}

func TestScreenRefusesOnlyWhenTheDeepPassConfirms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deep    string
		refused bool
	}{
		{"confirmed", `{"violation":true,"bucket":"threats","confidence":0.95,"reason":"a specific person, a specific act"}`, true},
		{"cleared", `{"violation":false,"bucket":"threats","confidence":0.1,"reason":"hyperbole"}`, false},
		{"below threshold", `{"violation":true,"bucket":"threats","confidence":0.6,"reason":"maybe"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClassifier{
				fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
				deep: []string{tc.deep},
			}
			p := intakePlugin(t, newFakeStore(), client, newFakeOps())

			refusal, err := p.Screen(context.Background(), "g1", "c1", "u1", "some genuinely threatening sentence")
			if err != nil {
				t.Fatalf("Screen: %v", err)
			}
			if (refusal != "") != tc.refused {
				t.Errorf("refusal = %q, want refused=%v", refusal, tc.refused)
			}
			if tc.refused && !strings.Contains(refusal, "a specific person") {
				t.Errorf("refusal %q does not tell the member why", refusal)
			}
			if fast, deep := client.counts(); fast != 1 || deep != 1 {
				t.Errorf("calls: fast=%d deep=%d, want 1 and 1", fast, deep)
			}
		})
	}
}

// A guild watching the filter in flag mode still gets a refusal: flag mode
// is about not touching members' own messages, and this one is the bot's.
func TestScreenIgnoresFlagMode(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"r"}`},
	}
	p := intakePlugin(t, store, client, newFakeOps())
	cfg, _ := store.Config(context.Background(), "g1")
	cfg.Mode = ModeFlag
	cfg.BucketActions[BucketThreats] = ActionFlag
	store.setConfig(cfg)

	if refusal, _ := p.Screen(context.Background(), "g1", "c1", "u1", "threat"); refusal == "" {
		t.Error("confirmed verdict cleared for posting because the guild is in flag mode")
	}
}

func TestScreenFallsBackOnAModelError(t *testing.T) {
	client := &fakeClassifier{fastErr: errors.New("openrouter: HTTP 503")}
	p := intakePlugin(t, newFakeStore(), client, newFakeOps())

	refusal, err := p.Screen(context.Background(), "g1", "c1", "u1", "an ordinary sentence")
	if refusal != "" {
		t.Errorf("refusal = %q on an outage; the caller decides, not this", refusal)
	}
	if err == nil {
		t.Error("no error reported for a failed fast pass, so the caller cannot tell it fell back")
	}
}

// The second whisper of the same text costs nothing either way.
func TestScreenReusesRememberedVerdicts(t *testing.T) {
	client := &fakeClassifier{
		fast: []string{`{"v":[{"i":1,"b":"threats","c":0.9}]}`, `{"v":[]}`},
		deep: []string{`{"violation":true,"bucket":"threats","confidence":0.95,"reason":"r"}`},
	}
	p := intakePlugin(t, newFakeStore(), client, newFakeOps())
	ctx := context.Background()

	if r, _ := p.Screen(ctx, "g1", "c1", "u1", "bad text"); r == "" {
		t.Fatal("first pass cleared a confirmed violation")
	}
	if r, _ := p.Screen(ctx, "g1", "c1", "u2", "bad text"); r == "" {
		t.Error("remembered violation cleared for another member")
	}
	if r, _ := p.Screen(ctx, "g1", "c1", "u1", "fine text"); r != "" {
		t.Fatalf("clean text refused: %q", r)
	}
	if r, _ := p.Screen(ctx, "g1", "c1", "u3", "fine text"); r != "" {
		t.Errorf("remembered clean text refused: %q", r)
	}
	if fast, deep := client.counts(); fast != 2 || deep != 1 {
		t.Errorf("calls: fast=%d deep=%d, want 2 and 1 across four screens", fast, deep)
	}
}
