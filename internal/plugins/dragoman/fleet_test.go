package dragoman

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/6586x57890143/cygnet/link"
)

func asked(text string) Asked {
	return Asked{Guild: guild, Channel: "101", Message: repost, User: author, Text: text}
}

func call(t *testing.T, r *rig, trust link.Trust, body any) (Answered, error) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.p.Answer(context.Background(), link.Call{From: "skua", Trust: trust, Body: b})
	if err != nil {
		return Answered{}, err
	}
	return out.(Answered), nil
}

// A bird's Dutch comes back English, with its flag, booked and audited
// like any translation, and nothing is posted by merlin.
func TestABirdsMessageIsTranslated(t *testing.T) {
	r := newRig(t)
	got, err := call(t, r, link.Signed, asked(dutch))
	if err != nil || got.Text != "the train's always late" || got.Flag != "🇳🇱" || got.Language != "nl" {
		t.Fatalf("%+v, %v", got, err)
	}
	if len(r.engine.reposts()) != 0 {
		t.Error("merlin posted a bird's message")
	}
	if a := r.audit.all(); !strings.Contains(a, `via="skua"`) || !strings.Contains(a, "[Translated](") {
		t.Errorf("audit: %s", a)
	}
}

// Every way of being left alone answers "" or a link code, and costs no
// call where the answer is known beforehand.
func TestABirdIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		set   func(*rig)
		trust link.Trust
		body  any
		code  link.Code
	}{
		"unsigned":       {nil, link.Untrusted, asked(dutch), link.Denied},
		"bad id":         {nil, link.Signed, Asked{Guild: "x", Channel: "1", Message: "1", User: "1", Text: dutch}, link.Invalid},
		"no text":        {nil, link.Signed, asked(""), link.Invalid},
		"too long":       {nil, link.Signed, asked(strings.Repeat("a", maxAsked+1)), link.Invalid},
		"unknown field":  {nil, link.Signed, map[string]string{"guild": guild, "evil": "1"}, link.Invalid},
		"held":           {func(r *rig) { r.held = true }, link.Signed, asked(dutch), link.Busy},
		"off":            {func(r *rig) { r.store.cfg.Enabled = false }, link.Signed, asked(dutch), ""},
		"plugin off":     {func(r *rig) { r.gate.off = true }, link.Signed, asked(dutch), ""},
		"exempt channel": {func(r *rig) { r.store.cfg.ExemptChannelIDs = []string{"101"} }, link.Signed, asked(dutch), ""},
		"opted out": {func(r *rig) {
			r.store.cfg.MemberControls, r.store.cfg.OptOutUserIDs = true, []string{author}
		}, link.Signed, asked(dutch), ""},
		"english": {nil, link.Signed, asked("the train is always late, that is not normal"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			if c.set != nil {
				c.set(r)
			}
			got, err := call(t, r, c.trust, c.body)
			if c.code != "" {
				if !errors.Is(err, link.Err(c.code)) {
					t.Fatalf("%+v, %v", got, err)
				}
			} else if err != nil || got.Text != "" {
				t.Fatalf("%+v, %v", got, err)
			}
			if r.engine.asked() != 0 {
				t.Errorf("%d calls", r.engine.asked())
			}
		})
	}
}

// What the model wrote and moderation would not post, or what has no
// harmless version, is left alone; so is a member over their share.
func TestABirdGetsNothingUnpostable(t *testing.T) {
	r := newRig(t)
	r.engine.out = strings.Replace(good, "always late", "always late token", 1)
	if got, err := call(t, r, link.Attested, asked(dutch)); err != nil || got.Text != "" {
		t.Errorf("unpublishable: %+v, %v", got, err)
	}
	r.engine.out = strings.Replace(good, `"harmful":""`, `"harmful":"child_safety"`, 1)
	if got, err := call(t, r, link.Attested, asked(dutch)); err != nil || got.Text != "" {
		t.Errorf("child safety: %+v, %v", got, err)
	}
	r.engine.out = `{"translate":false,"source":"en","confidence":0.9,"harmful":"","text":""}`
	if got, err := call(t, r, link.Attested, asked(dutch)); err != nil || got.Text != "" {
		t.Errorf("not for translating: %+v, %v", got, err)
	}
	r.engine.out = good
	for range memberMax {
		_, _ = call(t, r, link.Attested, asked(dutch))
	}
	if _, err := call(t, r, link.Attested, asked(dutch)); !errors.Is(err, link.Err(link.Busy)) {
		t.Errorf("over the share: %v", err)
	}
}
