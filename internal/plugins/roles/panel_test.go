package roles

import (
	"context"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/disgoorg/disgo/discord"
)

// Without a router to ask, nobody is allowed anything, so every control
// that changes something is drawn disabled; only navigation stays live.
func TestSettingsPanelDisablesWhatTheViewerCannotDo(t *testing.T) {
	p, _, _, _, _, _ := handlerFixture()
	for _, pg := range rolesPages {
		pn := p.panel(context.Background(), rolesInteraction("", "settings"), pg.key, "someone", "")
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

func TestSettingsOpensAndStaleControlsAnswer(t *testing.T) {
	p, _, _, _, _, _ := handlerFixture()
	rt := handlerSession(t)
	p.handleSettings(context.Background(), rolesInteraction("", "settings"))
	if len(rt.bodies) == 0 {
		t.Fatal("/roles settings left unanswered")
	}
	rt = handlerSession(t)
	set(p, panelJail+"gone", nil)
	if !rt.said("out of date") {
		t.Fatal("a stale control was not told so")
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
