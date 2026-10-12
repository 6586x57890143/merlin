package rotation

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/settings"
)

type noAuth struct{}

func (noAuth) ModRoleIDs(string) []string                    { return nil }
func (noAuth) AdminUserIDs(string) []string                  { return nil }
func (noAuth) ActionPolicy(string, string) core.ActionPolicy { return core.ActionPolicy{} }
func (noAuth) PluginEnabled(string, string) bool             { return true }

const boss = "boss"

// panelPlugin is a rotation plugin behind a real router, with boss as the
// bootstrap admin, and the channel rc already rotating.
func panelPlugin(t *testing.T) (*Plugin, *fakeOps, *fakeAudit, *fakeSettings) {
	t.Helper()
	ops, _, audit, p, _ := setupRotation(t, finiteRetentionRC())
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
	rc := finiteRetentionRC()
	pn := p.panel(context.Background(), paneltest.Click(boss, "", nil), "", "")
	text := strings.Join(texts(pn), "\n")
	if !strings.Contains(text, "<#"+rc.ChannelID+">") {
		t.Fatalf("list does not name %s:\n%s", rc.ChannelID, text)
	}
	if _, ok := paneltest.Selects(pn)[panelView+"ch"]; !ok {
		t.Fatal("no channel picker")
	}
}

// Members who may only look see every control, disabled, except navigation.
func TestNonAdminsCannotPressAnything(t *testing.T) {
	p, _, _, _ := panelPlugin(t)
	rc := finiteRetentionRC()
	for _, ch := range []string{"", rc.ChannelID, "fresh"} {
		pn := p.panel(context.Background(), paneltest.Click("member", "", nil), ch, "")
		for id, b := range paneltest.Buttons(pn) {
			if !strings.HasPrefix(id, panelView) && !b.Disabled {
				t.Errorf("%q: %s is pressable by a non-admin", ch, id)
			}
		}
		for id, m := range paneltest.Selects(pn) {
			if !strings.HasPrefix(id, panelView) && !m.Disabled {
				t.Errorf("%q: %s is usable by a non-admin", ch, id)
			}
		}
	}
}

func TestStartingRotationWaitsAFullInterval(t *testing.T) {
	p, _, audit, fs := panelPlugin(t)
	s, wire := paneltest.Session()
	p.handleForm(context.Background(), s, paneltest.Submit(boss, formStruct+"add:fresh", map[string]string{
		"interval": "2d", "retention": "", "notice": "off",
	}), formStruct+"add:fresh")

	rc, ok := fs.RotationChannel("g1", "fresh")
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
	ch := finiteRetentionRC().ChannelID
	s, wire := paneltest.Session()
	p.handleSet(context.Background(), s, paneltest.Click(boss, panelStruct+"remove:"+ch, nil), panelStruct+"remove:"+ch)
	if _, ok := fs.RotationChannel("g1", ch); !ok {
		t.Fatal("one press stopped it")
	}
	if !strings.Contains(wire.All(), panelStruct+"removeyes:"+ch) {
		t.Fatal("no confirm button offered")
	}
	p.handleSet(context.Background(), s, paneltest.Click(boss, panelStruct+"removeyes:"+ch, nil), panelStruct+"removeyes:"+ch)
	if _, ok := fs.RotationChannel("g1", ch); ok {
		t.Fatal("confirming did not stop it")
	}
}

func TestEveryRotationControlAnswers(t *testing.T) {
	p, _, _, _ := panelPlugin(t)
	ch := finiteRetentionRC().ChannelID
	msg := paneltest.Received(p.panel(context.Background(), paneltest.Click(boss, "", nil), "", ""))
	for _, tc := range []struct {
		id     string
		values []string
	}{
		{panelView + "ch", []string{ch}},
		{panelView + "home", nil},
		{panelStruct + "readers", []string{"r1"}},
		{panelStruct + "add:fresh", nil},
		{panelAdjust + "timing:" + ch, nil},
		{panelAdjust + "sticky:" + ch, nil},
		{panelAdjust + "stickyon:" + ch, nil},
		{panelAdjust + "stickyoff:" + ch, nil},
		{panelAdjust + "disclosure:" + ch, []string{string(settings.DisclosureGeneric)}},
		{panelAdjust + "category:" + ch, []string{"cat9"}},
		{panelAdjust + "timing:gone", nil},
		{panelAdjust + "nonsense:x", nil},
	} {
		s, wire := paneltest.Session()
		i := paneltest.Click(boss, tc.id, msg, tc.values...)
		if strings.HasPrefix(tc.id, panelView) {
			p.handleView(context.Background(), s, i, tc.id)
		} else {
			p.handleSet(context.Background(), s, i, tc.id)
		}
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", tc.id)
		}
	}
	for _, id := range []string{formAdjust + "timing:" + ch, formAdjust + "sticky:" + ch, formAdjust + "huh:" + ch} {
		s, wire := paneltest.Session()
		p.handleForm(context.Background(), s, paneltest.Submit(boss, id, map[string]string{"interval": "1d", "notice": "10m", "messages": "hi\n\nthere"}), id)
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", id)
		}
	}
	s, _ := paneltest.Session()
	p.handleOpen(context.Background(), s, paneltest.Click(boss, "", nil))
}

func texts(pn core.Panel) []string {
	var out []string
	var walk func([]discordgo.MessageComponent)
	walk = func(cs []discordgo.MessageComponent) {
		for _, c := range cs {
			switch v := c.(type) {
			case discordgo.Container:
				walk(v.Components)
			case discordgo.TextDisplay:
				out = append(out, v.Content)
			}
		}
	}
	walk(pn.Components())
	return out
}
