package adminconfig

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/settings"
)

// recordingSettings remembers every call the panel makes, and serves back
// whatever it was given as the guild's state.
type recordingSettings struct {
	fakeSettingsAdmin
	gs       settings.GuildSettings
	disabled map[string]bool
	calls    []string
}

func (r *recordingSettings) GuildSettings(string) settings.GuildSettings { return r.gs }
func (r *recordingSettings) PluginEnabled(_, name string) bool           { return !r.disabled[name] }
func (r *recordingSettings) rec(s string) error                          { r.calls = append(r.calls, s); return nil }
func (r *recordingSettings) AddAdmin(_ context.Context, _, id string) error {
	return r.rec("add-admin " + id)
}
func (r *recordingSettings) RemoveAdmin(_ context.Context, _, id string) error {
	return r.rec("remove-admin " + id)
}
func (r *recordingSettings) EnablePlugin(_ context.Context, _, n string) error {
	return r.rec("enable " + n)
}
func (r *recordingSettings) DisablePlugin(_ context.Context, _, n string) error {
	return r.rec("disable " + n)
}
func (r *recordingSettings) SetActionTier(_ context.Context, _, a string, t core.PermTier) error {
	return r.rec("tier " + a + " " + t.String())
}
func (r *recordingSettings) ClearActionTier(_ context.Context, _, a string) error {
	return r.rec("clear " + a)
}
func (r *recordingSettings) DenyOverride(_ context.Context, _, a, role, user string) error {
	return r.rec("deny " + a + " " + role + "/" + user)
}
func (r *recordingSettings) UndenyOverride(_ context.Context, _, a, role, user string) error {
	return r.rec("undeny " + a + " " + role + "/" + user)
}
func (r *recordingSettings) SetWritesPaused(_ context.Context, _ string, on bool) error {
	return r.rec("pause " + map[bool]string{true: "on", false: "off"}[on])
}

type nopAudit struct{}

func (nopAudit) Record(context.Context, string, string, string, string, string) error { return nil }

// bootstrapID is the operator; mod is a member the router would let read
// the panel but not change it. disgo's IDs are snowflakes, so every member,
// role and channel in these tests is numbered.
const (
	bootstrapID = "900"
	mod         = "901"
)

// newPanelPlugin is the plugin as main.go wires it, behind a real router, so
// the panel lists the plugins and actions that are really registered. Its
// session records what the plugin sends Discord as itself.
func newPanelPlugin(t *testing.T, st SettingsAdmin) *Plugin {
	t.Helper()
	log := discardLogger()
	perms := core.NewPermissions(nil, fakeAuthData{}, bootstrapID)
	router := core.NewCommandRouter(perms, fakeGate{}, log)
	router.HandleIx("other", "", core.PermSpec{Tier: core.TierMod, Action: "rotation.configure"}, nil)
	router.RegisterCommand("rotation", &discordgo.ApplicationCommand{Name: "other", Description: "x"})
	r, _ := paneltest.Rest()
	p := New(st, "does-not-exist.yaml", nil, nil)
	if err := p.Init(core.Deps{Commands: router, Logger: log, Perms: perms, Audit: nopAudit{}, Rest: r, BotID: 2}); err != nil {
		t.Fatal(err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatal(err)
	}
	return p
}

// click is user clicking customID on drawn (nil for a slash command or a
// message with no controls), submitting values.
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

// selects collects every select on a panel by CustomID.
func selects(pn core.Panel) map[string]discord.InteractiveComponent { return paneltest.IxSelects(pn) }

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

func options(c discord.InteractiveComponent) []discord.StringSelectMenuOption {
	if m, ok := c.(discord.StringSelectMenuComponent); ok {
		return m.Options
	}
	return nil
}

func TestEveryPageRendersWithTheRuleAndThePageSelect(t *testing.T) {
	p := newPanelPlugin(t, &recordingSettings{})
	for _, pg := range panelPages {
		pn := p.panel(context.Background(), click(t, bootstrapID, "", nil), pg.key, "", "")
		sel := selects(pn)
		menu, ok := sel[panelView+"page"]
		if !ok {
			t.Fatalf("%s: no page select", pg.key)
		}
		if !slices.ContainsFunc(options(menu), func(o discord.StringSelectMenuOption) bool { return o.Value == pg.key && o.Default }) {
			t.Errorf("%s: page select does not mark the current page", pg.key)
		}
	}
}

// A mod can read the whole panel, but every control that changes something
// is drawn disabled for them: the router would refuse the click anyway, and
// offering it would only make the refusal a surprise.
func TestModsSeeEveryControlDisabled(t *testing.T) {
	p := newPanelPlugin(t, &recordingSettings{})
	for _, pg := range panelPages {
		for id, m := range selects(p.panel(context.Background(), click(t, mod, "", nil), pg.key, "", "")) {
			if strings.HasPrefix(id, panelSet) && !disabled(m) {
				t.Errorf("%s: %s is live for someone who cannot change it", pg.key, id)
			}
		}
	}
	for id, m := range selects(p.panel(context.Background(), click(t, bootstrapID, "", nil), "access", "", "")) {
		if strings.HasPrefix(id, panelSet) && disabled(m) {
			t.Errorf("%s is disabled for the bootstrap admin", id)
		}
	}
}

// The second of two admins to submit must not remove what the first just
// added: it was never on their screen.
func TestAStalePanelOnlyUndoesWhatItShowed(t *testing.T) {
	st := &recordingSettings{gs: settings.GuildSettings{AdminUserIDs: []string{"1", "2"}}}
	p := newPanelPlugin(t, st)
	drawn := p.panel(context.Background(), click(t, bootstrapID, "", nil), "access", "", "")

	// Meanwhile 3 was added elsewhere; this member unpicks 1 and adds 4.
	st.gs.AdminUserIDs = []string{"1", "2", "3"}
	note := p.applyAdmins(context.Background(), click(t, bootstrapID, panelSet+"admins", &drawn, "2", "4"))

	if want := []string{"add-admin 4", "remove-admin 1"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
	if !strings.HasPrefix(note, "✓") {
		t.Errorf("note %q", note)
	}
}

func TestThePanelCannotSwitchItselfOff(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	pn := p.panel(context.Background(), click(t, bootstrapID, "", nil), "plugins", "", "")
	menu := selects(pn)[panelSet+"plugins"]
	for _, o := range options(menu) {
		if o.Value == p.Name() {
			t.Fatalf("%s is offered for switching off", p.Name())
		}
	}

	// A forged submit that drew adminconfig as on and unpicks it.
	forged := discord.StringSelectMenuComponent{CustomID: panelSet + "plugins", Options: []discord.StringSelectMenuOption{
		{Label: p.Name(), Value: p.Name(), Default: true}, {Label: "rotation", Value: "rotation", Default: true},
	}}
	msg := core.Panel{Parts: []discord.ContainerSubComponent{ui.Row(forged)}}
	p.applyPlugins(context.Background(), click(t, bootstrapID, panelSet+"plugins", &msg, []string{}...))
	if want := []string{"disable rotation"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
}

func TestConfigMutateStaysAdminOnThePanel(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	note := p.applyTier(context.Background(), click(t, bootstrapID, "", nil), actionMutate, "mod")
	if !strings.HasPrefix(note, "✗") || len(st.calls) != 0 {
		t.Fatalf("lowering %s went through: %q %v", actionMutate, note, st.calls)
	}
	if note := p.applyTier(context.Background(), click(t, bootstrapID, "", nil), "made.up", "mod"); !strings.HasPrefix(note, "✗") {
		t.Fatalf("an unregistered action was retiered: %q", note)
	}
	p.applyTier(context.Background(), click(t, bootstrapID, "", nil), "rotation.configure", "mod")
	p.applyTier(context.Background(), click(t, bootstrapID, "", nil), "rotation.configure", "default")
	if want := []string{"tier rotation.configure Admins + Mods", "clear rotation.configure"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
}

func TestPermissionsPageDrillsFromAreaToAction(t *testing.T) {
	p := newPanelPlugin(t, &recordingSettings{})
	ctx, i := context.Background(), click(t, bootstrapID, "", nil)
	if _, ok := selects(p.panel(ctx, i, "permissions", "", ""))[panelView+"action"]; ok {
		t.Error("action select shown before an area was picked")
	}
	if _, ok := selects(p.panel(ctx, i, "permissions", "rotation", ""))[panelView+"action"]; !ok {
		t.Error("no action select once an area was picked")
	}
	sel := selects(p.panel(ctx, i, "permissions", "rotation.configure", ""))
	for _, id := range []string{"allow-roles", "allow-users", "deny-roles", "deny-users"} {
		if _, ok := sel[panelSet+id+":rotation.configure"]; !ok {
			t.Errorf("no %s picker for the chosen action", id)
		}
	}
}

func TestBlockingIsAuditedPerTarget(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	id := panelSet + "deny-users:rotation.configure"
	drawn := core.Panel{Parts: []discord.ContainerSubComponent{ui.Row(ui.UserSelect(id, "", []string{"5"}, ui.MaxSelectValues, false))}}
	p.applyOverride(context.Background(), click(t, bootstrapID, id, &drawn, "6"), "deny-users", "rotation.configure")
	if want := []string{"deny rotation.configure /6", "undeny rotation.configure /5"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
}

func TestHandlePanelSetRedrawsWithTheOutcome(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	ix, wire := clickWire(t, bootstrapID, panelSet+"pause:on", nil)
	p.handlePanelSet(context.Background(), ix, panelSet+"pause:on")
	if !slices.Equal(st.calls, []string{"pause on"}) {
		t.Fatalf("calls %v", st.calls)
	}
	if !strings.Contains(wire.All(), "Paused") {
		t.Errorf("redraw does not say what happened:\n%.400s", wire.All())
	}
}

// Every control answers by redrawing the panel it was clicked on, never by
// leaving the interaction unanswered, including one whose CustomID is from a
// panel older than this build.
func TestEveryControlAnswers(t *testing.T) {
	st := &recordingSettings{gs: settings.GuildSettings{ModRoleIDs: []string{"21"}, AuditLogChannelID: "41"}}
	p := newPanelPlugin(t, st)
	drawn := p.panel(context.Background(), click(t, bootstrapID, "", nil), "access", "", "")
	for _, tc := range []struct {
		id     string
		values []string
	}{
		{panelView + "page", []string{"channels"}},
		{panelView + "area", []string{"rotation"}},
		{panelView + "action", []string{"rotation.configure"}},
		{panelSet + "modroles", []string{"21", "22"}},
		{panelSet + "admins", []string{"31"}},
		{panelSet + "audit", []string{"42"}},
		{panelSet + "status", []string{"43"}},
		{panelSet + "plugins", nil},
		{panelSet + "dryrun:on", nil},
		{panelSet + "dryrun:off", nil},
		{panelSet + "import", nil},
		{panelSet + "tier:admin:rotation.configure", nil},
		{panelSet + "allow-roles:rotation.configure", []string{"21"}},
		{panelSet + "setup", nil},
		{panelSet + "gone", nil},
	} {
		i, wire := clickWire(t, bootstrapID, tc.id, &drawn, tc.values...)
		if strings.HasPrefix(tc.id, panelView) {
			p.handlePanelView(context.Background(), i, tc.id)
		} else {
			p.handlePanelSet(context.Background(), i, tc.id)
		}
		if !wire.Answered() {
			t.Errorf("%s left the interaction unanswered", tc.id)
		}
	}
	ix, wire := paneltest.IxCommand(t, bootstrapID, "config", nil)
	p.handleConfig(context.Background(), ix)
	if !wire.Answered() {
		t.Error("/config left the interaction unanswered")
	}
}

func TestChangeNoteSaysNothingWhenNothingChanged(t *testing.T) {
	if n := changeNote(nil, nil, core.MentionUser, "admin"); n != "" {
		t.Errorf("got %q", n)
	}
	if n := changeNote([]string{"1"}, []string{"2"}, core.MentionUser, "admin"); n != "✓ Admins: added <@1>; removed <@2>." {
		t.Errorf("got %q", n)
	}
}
