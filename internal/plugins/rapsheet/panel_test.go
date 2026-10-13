package rapsheet

import (
	"context"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

// Without a router to ask, nobody may change anything, so every control
// that changes something is drawn disabled; only navigation stays live.
func TestSettingsPanelDisablesWhatTheViewerCannotDo(t *testing.T) {
	h := newHarness()
	pn := h.p.panel(context.Background(), interaction("settings"), CategorySpam, "")
	for id, m := range paneltest.Selects(pn) {
		if !strings.HasPrefix(id, panelView) && !m.Disabled {
			t.Errorf("%s is usable without permission", id)
		}
	}
	for id, b := range paneltest.Buttons(pn) {
		if !strings.HasPrefix(id, panelView) && !b.Disabled {
			t.Errorf("%s is pressable without permission", id)
		}
	}
	rt := stubSession()
	panelClick(h.p, panelSet+"gone")
	if !strings.Contains(rt.said(), "out of date") {
		t.Errorf("a stale control was not told so: %s", rt.said())
	}
}
