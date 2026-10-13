package rapsheet

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
	h := newHarness()
	pn := h.p.panel(context.Background(), interaction("settings"), CategorySpam, "")
	checked := 0
	for id, m := range paneltest.IxSelects(pn) {
		if !strings.HasPrefix(id, panelView) {
			checked++
			if !disabled(m) {
				t.Errorf("%s is usable without permission", id)
			}
		}
	}
	for id, b := range paneltest.IxButtons(pn) {
		if !strings.HasPrefix(id, panelView) {
			checked++
			if !b.Disabled {
				t.Errorf("%s is pressable without permission", id)
			}
		}
	}
	// The page has controls that change things, or this test checked nothing.
	if checked == 0 {
		t.Fatal("found no settings controls on the panel")
	}
	rt := stubSession()
	panelClick(h.p, panelSet+"gone")
	if !strings.Contains(rt.said(), "out of date") {
		t.Errorf("a stale control was not told so: %s", rt.said())
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
