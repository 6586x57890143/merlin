package roles

import (
	"context"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

// Without a router to ask, nobody is allowed anything, so every control
// that changes something is drawn disabled; only navigation stays live.
func TestSettingsPanelDisablesWhatTheViewerCannotDo(t *testing.T) {
	p, _, _, _, _, _ := handlerFixture()
	for _, pg := range rolesPages {
		pn := p.panel(context.Background(), paneltest.Click("actor", "", nil), pg.key, "someone", "")
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

func TestSettingsOpensAndStaleControlsAnswer(t *testing.T) {
	p, _, _, _, _, _ := handlerFixture()
	s, wire := paneltest.Session()
	p.handleSettings(context.Background(), s, rolesInteraction("", "settings"))
	if !wire.Answered() {
		t.Fatal("/roles settings left unanswered")
	}
	s, wire = paneltest.Session()
	set(p, s, panelJail+"gone", nil)
	if !strings.Contains(wire.All(), "out of date") {
		t.Fatal("a stale control was not told so")
	}
}
