package aimod

import (
	"context"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/disgoorg/disgo/discord"
)

// Without a router to ask, nobody may change anything, so every control
// that changes something is drawn disabled; only navigation stays live.
func TestSettingsPanelDisablesWhatTheViewerCannotDo(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	for _, pg := range aimodPages {
		pn := p.panel(context.Background(), interaction("g1", "", "settings"), pg.key, string(BucketHateSpeech), "")
		for id, m := range paneltest.IxSelects(pn) {
			if !strings.HasPrefix(id, panelView) && !disabled(m) {
				t.Errorf("%s: %s is usable without permission", pg.key, id)
			}
		}
		for id, b := range paneltest.IxButtons(pn) {
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
		i, wire := decodeIx("g1", 3, map[string]any{"custom_id": id, "component_type": 2}, nil)
		p.handlePanelSet(context.Background(), i, id)
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
		rows := []any{}
		for k, v := range fields {
			rows = append(rows, map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": k, "value": v}}})
		}
		i, wire := decodeIx("g1", 5, map[string]any{"custom_id": id, "components": rows}, nil)
		p.handleForm(context.Background(), i, id)
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", id)
		}
	}
	if cfg, _ := store.Config(context.Background(), "g1"); cfg.DailyBudgetUSD != 5 || cfg.EvidenceHours != 48 {
		t.Errorf("limits form saved budget %v evidence %v", cfg.DailyBudgetUSD, cfg.EvidenceHours)
	}
}

func disabled(c discord.InteractiveComponent) bool {
	switch m := c.(type) {
	case discord.StringSelectMenuComponent:
		return m.Disabled
	case discord.UserSelectMenuComponent:
		return m.Disabled
	case discord.RoleSelectMenuComponent:
		return m.Disabled
	case discord.ChannelSelectMenuComponent:
		return m.Disabled
	}
	return false
}
