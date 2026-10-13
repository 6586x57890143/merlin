package rotation

import (
	"context"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/settings"
)

type noAuth struct{}

func (noAuth) ModRoleIDs(string) []string                    { return nil }
func (noAuth) AdminUserIDs(string) []string                  { return nil }
func (noAuth) ActionPolicy(string, string) core.ActionPolicy { return core.ActionPolicy{} }
func (noAuth) PluginEnabled(string, string) bool             { return true }

// boss is the bootstrap admin and member anyone else; disgo IDs are
// snowflakes.
const boss, member = "900", "901"

// guild is where the panel tests' interactions come from.
const guild = paneltest.IxGuild

// panelRC is finiteRetentionRC in the guild the panel tests click in.
func panelRC() settings.RotationChannel {
	rc := finiteRetentionRC()
	rc.GuildID = guild
	return rc
}

// panelPlugin is a rotation plugin behind a real router, with boss as the
// bootstrap admin, and the channel rc already rotating.
func panelPlugin(t *testing.T) (*Plugin, *fakeOps, *fakeAudit, *fakeSettings) {
	t.Helper()
	ops, _, audit, p, _ := setupRotation(t, panelRC())
	p.sched = newFakeScheduler()
	p.commands = core.NewCommandRouter(core.NewPermissions(nil, noAuth{}, boss), noAuth{}, testLogger())
	p.registerCommands()
	if err := p.commands.Finalize(); err != nil {
		t.Fatal(err)
	}
	return p, ops, audit, p.settings.(*fakeSettings)
}

func TestListShowsEveryRotatingChannel(t *testing.T) {
	p, _, _, _ := panelPlugin(t)
	rc := panelRC()
	pn := p.panel(context.Background(), click(t, boss, "", nil), "", "")
	text := paneltest.IxTexts(pn)
	if !strings.Contains(text, "<#"+rc.ChannelID+">") {
		t.Fatalf("list does not name %s:\n%s", rc.ChannelID, text)
	}
	if _, ok := paneltest.IxSelects(pn)[panelView+"ch"]; !ok {
		t.Fatal("no channel picker")
	}
}

// Members who may only look see every control, disabled, except navigation.
func TestNonAdminsCannotPressAnything(t *testing.T) {
	p, _, _, _ := panelPlugin(t)
	rc := panelRC()
	for _, ch := range []string{"", rc.ChannelID, "305"} {
		pn := p.panel(context.Background(), click(t, member, "", nil), ch, "")
		for id, b := range paneltest.IxButtons(pn) {
			if !strings.HasPrefix(id, panelView) && !b.Disabled {
				t.Errorf("%q: %s is pressable by a non-admin", ch, id)
			}
		}
		for id, m := range paneltest.IxSelects(pn) {
			if !strings.HasPrefix(id, panelView) && !disabled(m) {
				t.Errorf("%q: %s is usable by a non-admin", ch, id)
			}
		}
	}
}

func TestStartingRotationWaitsAFullInterval(t *testing.T) {
	p, _, audit, fs := panelPlugin(t)
	ix, wire := paneltest.IxSubmit(t, boss, formStruct+"add:305", map[string]string{
		"interval": "2d", "retention": "", "notice": "off",
	})
	p.handleForm(context.Background(), ix, formStruct+"add:305")

	rc, ok := fs.RotationChannel(guild, "305")
	if !ok {
		t.Fatalf("not saved; panel said:\n%.600s", wire.All())
	}
	if rc.IntervalMinutes != 2*24*60 || rc.RetentionHours != nil || rc.NoticeLeadMinutes != 0 || rc.ArchiveCategoryID == "" {
		t.Fatalf("saved %+v", rc)
	}
	if len(p.sched.(*fakeScheduler).seedCalls) != 1 {
		t.Error("a channel just added would fire on the next tick")
	}
	if len(audit.records) != 1 || audit.records[0].action != "rotation.add" {
		t.Errorf("audit %+v", audit.records)
	}
}

// A duration without a unit is refused: a bare "3" was once read as hours
// when days were meant, and deletion has no undo.
func TestTimingsNeedUnits(t *testing.T) {
	rc := settings.RotationChannel{}
	if err := applyTimings(&rc, map[string]string{"interval": "3", "notice": "off"}); err == nil {
		t.Fatal("unitless interval accepted")
	}
	if err := applyTimings(&rc, map[string]string{"interval": "3d", "retention": "5", "notice": "off"}); err == nil {
		t.Fatal("unitless retention accepted")
	}
}

func TestStoppingTakesTwoPresses(t *testing.T) {
	p, _, _, fs := panelPlugin(t)
	ch := panelRC().ChannelID
	ix, wire := clickWire(t, boss, panelStruct+"remove:"+ch, nil)
	p.handleSet(context.Background(), ix, panelStruct+"remove:"+ch)
	if _, ok := fs.RotationChannel(guild, ch); !ok {
		t.Fatal("one press stopped it")
	}
	if !strings.Contains(wire.All(), panelStruct+"removeyes:"+ch) {
		t.Fatal("no confirm button offered")
	}
	p.handleSet(context.Background(), click(t, boss, panelStruct+"removeyes:"+ch, nil), panelStruct+"removeyes:"+ch)
	if _, ok := fs.RotationChannel(guild, ch); ok {
		t.Fatal("confirming did not stop it")
	}
}

func TestEveryRotationControlAnswers(t *testing.T) {
	p, _, _, _ := panelPlugin(t)
	ch := panelRC().ChannelID
	msg := p.panel(context.Background(), click(t, boss, "", nil), "", "")
	for _, tc := range []struct {
		id     string
		values []string
	}{
		{panelView + "ch", []string{ch}},
		{panelView + "home", nil},
		{panelStruct + "readers", []string{"21"}},
		{panelStruct + "add:305", nil},
		{panelAdjust + "timing:" + ch, nil},
		{panelAdjust + "sticky:" + ch, nil},
		{panelAdjust + "stickyon:" + ch, nil},
		{panelAdjust + "stickyoff:" + ch, nil},
		{panelAdjust + "disclosure:" + ch, []string{string(settings.DisclosureGeneric)}},
		{panelAdjust + "category:" + ch, []string{"29"}},
		{panelAdjust + "timing:gone", nil},
		{panelAdjust + "nonsense:x", nil},
	} {
		i, wire := clickWire(t, boss, tc.id, &msg, tc.values...)
		if strings.HasPrefix(tc.id, panelView) {
			p.handleView(context.Background(), i, tc.id)
		} else {
			p.handleSet(context.Background(), i, tc.id)
		}
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", tc.id)
		}
	}
	for _, id := range []string{formAdjust + "timing:" + ch, formAdjust + "sticky:" + ch, formAdjust + "huh:" + ch} {
		ix, wire := paneltest.IxSubmit(t, boss, id, map[string]string{"interval": "1d", "notice": "10m", "messages": "hi\n\nthere"})
		p.handleForm(context.Background(), ix, id)
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", id)
		}
	}
	ix, wire := paneltest.IxCommand(t, boss, "rotation", nil)
	p.handleOpen(context.Background(), ix)
	if !wire.Answered() {
		t.Error("/rotation left the interaction unanswered")
	}
}

// click is user clicking customID on drawn (nil for no controls) with values.
func click(t *testing.T, user, customID string, drawn *core.Panel, values ...string) *core.Ix {
	t.Helper()
	ix, _ := clickWire(t, user, customID, drawn, values...)
	return ix
}

func clickWire(t *testing.T, user, customID string, drawn *core.Panel, values ...string) (*core.Ix, *paneltest.Wire) {
	t.Helper()
	kind := 2
	if values != nil {
		kind = 3
	}
	return paneltest.IxComponent(t, user, customID, kind, drawn, values...)
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
