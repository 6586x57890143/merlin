package adminconfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
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
func (r *recordingSettings) PluginEnabled(_, name string) bool          { return !r.disabled[name] }
func (r *recordingSettings) rec(s string) error                         { r.calls = append(r.calls, s); return nil }
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

const bootstrapID = "boss"

// newPanelPlugin is the plugin as main.go wires it, behind a real router, so
// the panel lists the plugins and actions that are really registered.
func newPanelPlugin(t *testing.T, st SettingsAdmin) *Plugin {
	t.Helper()
	log := discardLogger()
	perms := core.NewPermissions(nil, fakeAuthData{}, bootstrapID)
	router := core.NewCommandRouter(perms, fakeGate{}, log)
	router.Handle("other", "", core.PermSpec{Tier: core.TierMod, Action: "rotation.configure"}, nil)
	router.RegisterCommand("rotation", &discordgo.ApplicationCommand{Name: "other", Description: "x"})
	p := New(st, "does-not-exist.yaml", nil, nil)
	if err := p.Init(core.Deps{Commands: router, Logger: log, Perms: perms, Audit: nopAudit{}}); err != nil {
		t.Fatal(err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatal(err)
	}
	return p
}

func click(user, customID string, msg *discordgo.Message, values ...string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionMessageComponent,
		GuildID: "g",
		Member:  &discordgo.Member{User: &discordgo.User{ID: user}},
		Message: msg,
		Data:    discordgo.MessageComponentInteractionData{CustomID: customID, Values: values},
	}}
}

// asReceived is a panel the way Discord hands it back on a click.
func asReceived(t *testing.T, pn core.Panel) *discordgo.Message {
	t.Helper()
	raw, err := json.Marshal(pn.Components())
	if err != nil {
		t.Fatal(err)
	}
	var m discordgo.Message
	if err := json.Unmarshal([]byte(`{"components":`+string(raw)+`}`), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// selects collects every select on a panel by CustomID.
func selects(cs []discordgo.MessageComponent) map[string]discordgo.SelectMenu {
	out := map[string]discordgo.SelectMenu{}
	var walk func([]discordgo.MessageComponent)
	walk = func(cs []discordgo.MessageComponent) {
		for _, c := range cs {
			switch v := c.(type) {
			case discordgo.Container:
				walk(v.Components)
			case discordgo.ActionsRow:
				walk(v.Components)
			case discordgo.SelectMenu:
				out[v.CustomID] = v
			}
		}
	}
	walk(cs)
	return out
}

func TestEveryPageRendersWithTheRuleAndThePageSelect(t *testing.T) {
	p := newPanelPlugin(t, &recordingSettings{})
	for _, pg := range panelPages {
		pn := p.panel(context.Background(), click(bootstrapID, "", nil), pg.key, "", "")
		sel := selects(pn.Components())
		menu, ok := sel[panelView+"page"]
		if !ok {
			t.Fatalf("%s: no page select", pg.key)
		}
		if !slices.ContainsFunc(menu.Options, func(o discordgo.SelectMenuOption) bool { return o.Value == pg.key && o.Default }) {
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
		for id, m := range selects(p.panel(context.Background(), click("a-mod", "", nil), pg.key, "", "").Components()) {
			if strings.HasPrefix(id, panelSet) && !m.Disabled {
				t.Errorf("%s: %s is live for someone who cannot change it", pg.key, id)
			}
		}
	}
	for id, m := range selects(p.panel(context.Background(), click(bootstrapID, "", nil), "access", "", "").Components()) {
		if strings.HasPrefix(id, panelSet) && m.Disabled {
			t.Errorf("%s is disabled for the bootstrap admin", id)
		}
	}
}

// The second of two admins to submit must not remove what the first just
// added: it was never on their screen.
func TestAStalePanelOnlyUndoesWhatItShowed(t *testing.T) {
	st := &recordingSettings{gs: settings.GuildSettings{AdminUserIDs: []string{"a", "b"}}}
	p := newPanelPlugin(t, st)
	drawn := asReceived(t, p.panel(context.Background(), click(bootstrapID, "", nil), "access", "", ""))

	// Meanwhile "c" was added elsewhere; this member unpicks "a" and adds "d".
	st.gs.AdminUserIDs = []string{"a", "b", "c"}
	note := p.applyAdmins(context.Background(), click(bootstrapID, panelSet+"admins", drawn, "b", "d"))

	if want := []string{"add-admin d", "remove-admin a"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
	if !strings.HasPrefix(note, "✓") {
		t.Errorf("note %q", note)
	}
}

func TestThePanelCannotSwitchItselfOff(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	pn := p.panel(context.Background(), click(bootstrapID, "", nil), "plugins", "", "")
	menu := selects(pn.Components())[panelSet+"plugins"]
	for _, o := range menu.Options {
		if o.Value == p.Name() {
			t.Fatalf("%s is offered for switching off", p.Name())
		}
	}

	// A forged submit that drew adminconfig as on and unpicks it.
	forged := discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: panelSet + "plugins", Options: []discordgo.SelectMenuOption{
		{Label: p.Name(), Value: p.Name(), Default: true}, {Label: "rotation", Value: "rotation", Default: true},
	}}
	msg := asReceived(t, core.Panel{Body: []discordgo.MessageComponent{core.Row(forged)}})
	p.applyPlugins(context.Background(), click(bootstrapID, panelSet+"plugins", msg))
	if want := []string{"disable rotation"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
}

func TestConfigMutateStaysAdminOnThePanel(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	note := p.applyTier(context.Background(), click(bootstrapID, "", nil), actionMutate, "mod")
	if !strings.HasPrefix(note, "✗") || len(st.calls) != 0 {
		t.Fatalf("lowering %s went through: %q %v", actionMutate, note, st.calls)
	}
	if note := p.applyTier(context.Background(), click(bootstrapID, "", nil), "made.up", "mod"); !strings.HasPrefix(note, "✗") {
		t.Fatalf("an unregistered action was retiered: %q", note)
	}
	p.applyTier(context.Background(), click(bootstrapID, "", nil), "rotation.configure", "mod")
	p.applyTier(context.Background(), click(bootstrapID, "", nil), "rotation.configure", "default")
	if want := []string{"tier rotation.configure Admins + Mods", "clear rotation.configure"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
}

func TestPermissionsPageDrillsFromAreaToAction(t *testing.T) {
	p := newPanelPlugin(t, &recordingSettings{})
	ctx, i := context.Background(), click(bootstrapID, "", nil)
	if _, ok := selects(p.panel(ctx, i, "permissions", "", "").Components())[panelView+"action"]; ok {
		t.Error("action select shown before an area was picked")
	}
	if _, ok := selects(p.panel(ctx, i, "permissions", "rotation", "").Components())[panelView+"action"]; !ok {
		t.Error("no action select once an area was picked")
	}
	sel := selects(p.panel(ctx, i, "permissions", "rotation.configure", "").Components())
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
	drawn := asReceived(t, core.Panel{Body: []discordgo.MessageComponent{core.Row(core.EntitySelect(discordgo.UserSelectMenu, id, "", []string{"x"}, core.MaxSelectValues, false))}})
	p.applyOverride(context.Background(), click(bootstrapID, id, drawn, "y"), "deny-users", "rotation.configure")
	if want := []string{"deny rotation.configure /y", "undeny rotation.configure /x"}; !slices.Equal(st.calls, want) {
		t.Fatalf("calls %v, want %v", st.calls, want)
	}
}

func TestHandlePanelSetRedrawsWithTheOutcome(t *testing.T) {
	st := &recordingSettings{}
	p := newPanelPlugin(t, st)
	var body string
	s, _ := discordgo.New("Bot x")
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) { body = readAll(r) })}
	p.handlePanelSet(context.Background(), s, click(bootstrapID, panelSet+"pause:on", nil), panelSet+"pause:on")
	if !slices.Equal(st.calls, []string{"pause on"}) {
		t.Fatalf("calls %v", st.calls)
	}
	if !strings.Contains(body, "Paused") {
		t.Errorf("redraw does not say what happened:\n%.400s", body)
	}
}

type roundTrip func(*http.Request)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	f(r)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

func readAll(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// Every control answers by redrawing the panel it was clicked on, never by
// leaving the interaction unanswered, including one whose CustomID is from a
// panel older than this build.
func TestEveryControlAnswers(t *testing.T) {
	st := &recordingSettings{gs: settings.GuildSettings{ModRoleIDs: []string{"r"}, AuditLogChannelID: "c"}}
	p := newPanelPlugin(t, st)
	drawn := asReceived(t, p.panel(context.Background(), click(bootstrapID, "", nil), "access", "", ""))
	for _, tc := range []struct {
		id     string
		values []string
	}{
		{panelView + "page", []string{"channels"}},
		{panelView + "area", []string{"rotation"}},
		{panelView + "action", []string{"rotation.configure"}},
		{panelSet + "modroles", []string{"r", "r2"}},
		{panelSet + "admins", []string{"u"}},
		{panelSet + "audit", []string{"c2"}},
		{panelSet + "status", []string{"c3"}},
		{panelSet + "plugins", nil},
		{panelSet + "dryrun:on", nil},
		{panelSet + "dryrun:off", nil},
		{panelSet + "import", nil},
		{panelSet + "tier:admin:rotation.configure", nil},
		{panelSet + "allow-roles:rotation.configure", []string{"r"}},
		{panelSet + "setup", nil},
		{panelSet + "gone", nil},
	} {
		answered := false
		s, _ := discordgo.New("Bot x")
		s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) {
			if strings.Contains(r.URL.Path, "/callback") {
				answered = true
			}
		})}
		i := click(bootstrapID, tc.id, drawn, tc.values...)
		if strings.HasPrefix(tc.id, panelView) {
			p.handlePanelView(context.Background(), s, i, tc.id)
		} else {
			p.handlePanelSet(context.Background(), s, i, tc.id)
		}
		if !answered {
			t.Errorf("%s left the interaction unanswered", tc.id)
		}
	}
	s, _ := discordgo.New("Bot x")
	s.Client = &http.Client{Transport: roundTrip(func(*http.Request) {})}
	p.handleConfig(context.Background(), s, click(bootstrapID, "", nil))
}

func TestChangeNoteSaysNothingWhenNothingChanged(t *testing.T) {
	if n := changeNote(nil, nil, core.MentionUser, "admin"); n != "" {
		t.Errorf("got %q", n)
	}
	if n := changeNote([]string{"1"}, []string{"2"}, core.MentionUser, "admin"); n != "✓ Admins: added <@1>; removed <@2>." {
		t.Errorf("got %q", n)
	}
}
