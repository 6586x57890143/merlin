package dragoman

import (
	"errors"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

const (
	unsure  = `{"translate":true,"source":"nl","confidence":0.85,"harmful":"","text":"the train is tardy"}`
	leaving = `{"translate":false,"source":"en","confidence":0.9,"harmful":"","text":""}`
	// odd is past the gate on its words alone: weak evidence.
	odd = "blorp zeng wuvvle"
)

// What each message costs: which rungs are asked, and whose words are posted.
func TestTheLadder(t *testing.T) {
	const sure, tardy = "the train's always late", "the train is tardy"
	for name, tc := range map[string]struct {
		text   string
		change func(*rig)
		rungs  string
		posted string
	}{
		"a good answer is the only call":     {dutch, func(*rig) {}, "model", sure},
		"a doubtful one is asked again":      {dutch, func(r *rig) { r.engine.outs = []string{unsure} }, "model careful", sure},
		"the careful answer is the last":     {dutch, func(r *rig) { r.engine.outs = []string{unsure, unsure} }, "model careful", tardy},
		"an unreadable one is asked again":   {dutch, func(r *rig) { r.engine.outs = []string{"sure!"} }, "model careful", sure},
		"plain dutch isn't left on one no":   {dutch, func(r *rig) { r.engine.outs = []string{leaving} }, "model careful", sure},
		"what was never sure is":             {odd, func(r *rig) { r.engine.outs = []string{leaving} }, "model", ""},
		"the pre-filter keeps the odd":       {odd, func(r *rig) { r.engine.skip = true }, "", ""},
		"the pre-filter has no say on dutch": {dutch, func(r *rig) { r.engine.skip = true }, "model", sure},
	} {
		r := newRig(t)
		tc.change(r)
		r.say(msg(tc.text))
		var rungs []string
		for _, q := range r.engine.prompts {
			switch {
			case q.Brief:
				rungs = append(rungs, "model")
			default:
				rungs = append(rungs, "careful")
			}
		}
		if got := strings.Join(rungs, " "); got != tc.rungs {
			t.Errorf("%s: asked %q, want %q", name, got, tc.rungs)
		}
		posted := ""
		if p := r.engine.reposts(); len(p) == 1 {
			posted = p[0].Text
		}
		if posted != tc.posted {
			t.Errorf("%s: posted %q, want %q", name, posted, tc.posted)
		}
	}
}

// The second call knows what was wrong with the first, and a model picked by
// the guild is the one asked both times.
func TestAskingAgain(t *testing.T) {
	r := newRig(t)
	r.store.cfg.Model, r.engine.outs, r.engine.cost = "a/b", []string{unsure}, 0.001
	r.say(msg(dutch))
	if q := r.engine.prompts; len(q) != 2 || q[0].Models[0] != "a/b" || q[1].Models[0] != "a/b" ||
		!strings.Contains(q[1].User, "you were not sure of it") || !strings.Contains(q[1].User, "tardy") || q[1].System != q[0].System {
		t.Errorf("prompts %+v", q)
	}
	if u := r.store.used; u[rungModel] != (Usage{1, 0.001}) || u[rungCareful] != (Usage{1, 0.001}) {
		t.Errorf("used %+v", u)
	}
	if log := r.audit.all(); !strings.Contains(log, `rung="careful"`) {
		t.Errorf("audit %s", log)
	}

	// A model picked while there was an OpenRouter key is no use without one.
	r = newRig(t)
	r.store.cfg.Model, r.engine.orca = "a/b", true
	r.say(msg(dutch))
	if q := r.engine.prompts; len(q) != 1 || q[0].Models != nil {
		t.Errorf("prompts %+v", q)
	}

	// A doubt is never worth losing a publishable translation over.
	r = newRig(t)
	r.engine.outs, r.engine.askErr = []string{unsure}, quiet{}
	r.say(msg(dutch))
	if p := r.engine.reposts(); len(p) != 1 || p[0].Text != "the train is tardy" {
		t.Errorf("posted %+v", p)
	}
	if u := r.store.used; len(u) != 1 {
		t.Errorf("a call that was never made was counted: %+v", u)
	}
}

func TestThePreFilterIsTaughtByTheModel(t *testing.T) {
	r := newRig(t)
	r.say(msg(dutch))
	r.engine.outs = []string{leaving}
	r.say(msg(odd))
	r.engine.askErr = errors.New("boom")
	r.say(msg(odd))
	if got := r.engine.taught; len(got) != 2 || !got[0] || got[1] {
		t.Errorf("taught %v, want one worth it and one not, and nothing of a failure", got)
	}
}

func TestDoubt(t *testing.T) {
	long := strings.Repeat("de trein is laat ", 8)
	for _, tc := range []struct {
		masked, text string
		want         bool
	}{
		{"de trein is laat", "the train is late", false},
		{"om 14:30 in zaal 2", "at 14:30 in room 2", false},
		{"om 14:30 in zaal 2", "at 14:30 in the room", true},
		{"kijk " + tok("1"), "look " + tok("1"), false},
		{"een\ntwee", "one\ntwo", false},
		{"een\ntwee", "one two", true},
		{"ich bin müde", "ich bin nicht müde aber das ist gut", true},
		{"これはペンです", "これはペンです", true},
		{long, "the train", true},
		{"これはペンです", "this is a pen", false},
	} {
		got := doubt(translation{masked: tc.text, confidence: 1}, tc.masked)
		if (got != "") != tc.want {
			t.Errorf("%q as %q: %q", tc.masked, tc.text, got)
		}
	}
	if doubt(translation{masked: "x", confidence: 1, policy: "threats"}, "x") == "" {
		t.Error("harm went unconfirmed")
	}
	if !dropsG("he stay complainin man") || dropsG("the train begins again within the cabin") {
		t.Error("dropsG")
	}
}

func TestPickingAModel(t *testing.T) {
	r := newRig(t)
	r.engine.prices = map[string]core.ModelPrice{"z-ai/glm-5.3-flash": {Prompt: 0.15, Completion: 0.5}, "o/ther": {Prompt: 1, Completion: 1}}
	pick := func(v string) (*core.Ix, *paneltest.Wire) { return paneltest.IxPick(t, op, panelSet+"model", nil, v) }
	for _, tc := range []struct{ pick, want, note string }{
		{"z-ai/glm-5.3-flash", "z-ai/glm-5.3-flash", "about $0.30 per 1,000"},
		{"no/where", "z-ai/glm-5.3-flash", "no zero data retention"},
		{modelDefault, "", "deep pass"},
	} {
		ix, w := pick(tc.pick)
		r.p.handlePanelSet(t.Context(), ix, panelSet+"model")
		if r.store.cfg.Model != tc.want || !strings.Contains(w.All(), tc.note) {
			t.Errorf("%s: model %q, said %q", tc.pick, r.store.cfg.Model, w.All())
		}
	}
	ix, w := pick(modelOther)
	r.p.handlePanelSet(t.Context(), ix, panelSet+"model")
	if !strings.Contains(w.All(), formModel) {
		t.Errorf("no form: %s", w.All())
	}
	ix, _ = paneltest.IxSubmit(t, op, formModel, map[string]string{"model": " o/ther "})
	r.p.handleModelForm(t.Context(), ix, formModel)
	if r.store.cfg.Model != "o/ther" || !strings.Contains(r.audit.all(), "dragoman.model_set|AI moderation's deep pass|o/ther") {
		t.Errorf("model %q, audit %s", r.store.cfg.Model, r.audit.all())
	}
	// A pick that isn't on the list is still shown as picked.
	m := r.p.modelPicker(t.Context(), guild, r.store.cfg, false)
	shown := false
	for _, o := range m.Options {
		shown = shown || (o.Default && o.Value == "o/ther")
	}
	if !shown {
		t.Errorf("options %+v", m.Options)
	}
	// Prices that can't be read save nothing, and neither does a database
	// that isn't there.
	r.engine.prices = nil
	ix, _ = pick("z-ai/glm-5.3-flash")
	r.p.handlePanelSet(t.Context(), ix, panelSet+"model")
	if r.store.cfg.Model != "o/ther" {
		t.Error("an unchecked model was saved")
	}
}

// The panel tells translation's own cost from the budget it shares.
func TestThePanelSaysWhatItCost(t *testing.T) {
	r := newRig(t)
	r.store.used = map[string]Usage{rungModel: {5, 0.01}, rungCareful: {1, 0.005}, rungSkipped: {7, 0}}
	ix, _ := paneltest.IxCommand(t, op, "dragoman", nil)
	got := paneltest.IxTexts(r.p.panel(t.Context(), ix, ""))
	for _, want := range []string{"$0.0150", "6, 1 asked again", "7 calls saved", "$0.10 of $0.50", "$0.40 all ai"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
}
