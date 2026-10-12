package aimod

import (
	"context"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

// Without a router to ask, nobody may change anything, so every control
// that changes something is drawn disabled; only navigation stays live.
func TestSettingsPanelDisablesWhatTheViewerCannotDo(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	for _, pg := range aimodPages {
		pn := p.panel(context.Background(), interaction("g1", "", "settings"), pg.key, string(BucketHateSpeech), "")
		for id, m := range paneltest.Selects(pn) {
			if !strings.HasPrefix(id, panelView) && !m.Disabled {
				t.Errorf("%s: %s is usable without permission", pg.key, id)
			}
		}
		for id, b := range paneltest.Buttons(pn) {
			if !strings.HasPrefix(id, panelView) && !b.Disabled {
				t.Errorf("%s: %s is pressable without permission", pg.key, id)
			}
		}
	}
}

// Every control and form answers, including a stale one, so none can leave
// a member looking at "the application did not respond".
func TestEverySettingsControlAnswers(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	for _, id := range []string{
		panelCfg + "key", panelCfg + "limits", panelCfg + "models", panelPol + "word",
		panelCfg + "mode", panelCfg + "exch", panelPol + "sanction", panelPol + "triage", panelPol + "optout:on",
		panelPol + "bucket:" + string(BucketHateSpeech) + ":" + string(ActionFlag), panelPol + "unword",
		panelPol + "calmode", panelPol + "calapply", panelPol + "calclear", panelPol + "calnow", panelPol + "gone",
	} {
		s, wire := paneltest.Session()
		p.handlePanelSet(context.Background(), s, paneltest.Click("actor", id, nil), id)
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", id)
		}
	}
	for id, fields := range map[string]map[string]string{
		formCfg + "key":    {"key": "nonsense"},
		formCfg + "limits": {"usd": "5", "hours": "48"},
		formCfg + "models": {"fast": "default", "deep": "default"},
		formPol + "word":   {"word": "work", "replacement": "the mines", "noun": "yes"},
		formPol + "huh":    {},
	} {
		s, wire := paneltest.Session()
		i := paneltest.Submit("actor", id, fields)
		i.GuildID = "g1"
		p.handleForm(context.Background(), s, i, id)
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", id)
		}
	}
	if cfg, _ := store.Config(context.Background(), "g1"); cfg.DailyBudgetUSD != 5 || cfg.EvidenceHours != 48 {
		t.Errorf("limits form saved budget %v evidence %v", cfg.DailyBudgetUSD, cfg.EvidenceHours)
	}
}
