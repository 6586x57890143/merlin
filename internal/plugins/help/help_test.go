package help

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/fleet"
)

type toggles struct {
	off   map[string]bool
	calls []string
}

func (t *toggles) PluginEnabled(_, name string) bool { return !t.off[name] }
func (t *toggles) EnablePlugin(_ context.Context, _, name string) error {
	t.calls = append(t.calls, "on "+name)
	delete(t.off, name)
	return nil
}
func (t *toggles) DisablePlugin(_ context.Context, _, name string) error {
	t.calls = append(t.calls, "off "+name)
	t.off[name] = true
	return nil
}

type audit struct{ actions []string }

func (a *audit) Record(_ context.Context, _, _, action, _, _ string) error {
	a.actions = append(a.actions, action)
	return nil
}

type auth struct{}

func (auth) ModRoleIDs(string) []string                    { return nil }
func (auth) AdminUserIDs(string) []string                  { return nil }
func (auth) ActionPolicy(string, string) core.ActionPolicy { return core.ActionPolicy{} }
func (auth) PluginEnabled(string, string) bool             { return true }

// fake is a plugin with a page and one command with a mod and an admin leaf.
type fake struct{ name string }

func (f fake) Name() string                   { return f.name }
func (f fake) Init(d core.Deps) error         { return nil }
func (f fake) Start(context.Context) error    { return nil }
func (f fake) Shutdown(context.Context) error { return nil }
func (f fake) Help() core.Help {
	return core.Help{Title: strings.ToUpper(f.name[:1]) + f.name[1:], Line: "does " + f.name, About: "about " + f.name}
}

// boss is the bootstrap identity; member is anybody else.
const boss, member = "900", "11"

func setup(t *testing.T) (*Plugin, *toggles, *audit) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := core.NewCommandRouter(core.NewPermissions(nil, auth{}, boss), auth{}, log)
	router.RegisterCommand("rotation", &discordgo.ApplicationCommand{Name: "rotation", Description: "x", Options: []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "x"},
		{Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "configure", Description: "x", Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "x"},
		}},
	}})
	router.Handle("rotation", "list", core.PermSpec{Tier: core.TierMod, Action: "rotation.list"}, nil)
	router.Handle("rotation", "configure/add", core.PermSpec{Tier: core.TierAdmin, Action: "rotation.x"}, nil)
	tg, au := &toggles{off: map[string]bool{"contest": true}}, &audit{}
	var p *Plugin
	p = New(func() []core.Plugin {
		return []core.Plugin{fake{"rotation"}, fake{"contest"}, fake{"adminconfig"}, p}
	}, tg)
	if err := p.Init(core.Deps{Commands: router, Logger: log, Audit: au}); err != nil {
		t.Fatal(err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatal(err)
	}
	return p, tg, au
}

func text(pn core.Panel) string { return paneltest.IxTexts(pn) }

// A plugin switched off here is listed to its admins, marked, and hidden
// from everyone else; a posted guide is read by everyone, so it lists only
// what is on.
func TestIndexListsWhatTheViewerMaySee(t *testing.T) {
	p, _, _ := setup(t)
	admin := text(p.index("g1", true))
	if !strings.Contains(admin, "**Contest** · `off here`") || !strings.Contains(admin, "-# does rotation") {
		t.Errorf("admin index:\n%s", admin)
	}
	// The build named here is the one cygnet is told, stamped in by the
	// Dockerfile, so the two can never disagree about what is running.
	if !strings.Contains(admin, "build `"+fleet.Build()+"`") {
		t.Errorf("index does not name fleet's build:\n%s", admin)
	}
	if member := text(p.index("g1", false)); strings.Contains(member, "Contest") {
		t.Errorf("a switched-off plugin was listed to a member:\n%s", member)
	}
}

func TestPageShowsWhoMayRunEachCommand(t *testing.T) {
	p, _, _ := setup(t)
	pn, ok := p.page("g1", "rotation", false)
	if !ok {
		t.Fatal("no page")
	}
	got := text(pn)
	for _, want := range []string{"about rotation", "/rotation", "├ list", "mod", "└ configure add", "admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("page missing %q:\n%s", want, got)
		}
	}
	if _, ok := paneltest.IxButtons(pn)[switchPrefix+"off:rotation"]; ok {
		t.Error("a member was offered the switch")
	}
	pn, _ = p.page("g1", "rotation", true)
	if _, ok := paneltest.IxButtons(pn)[switchPrefix+"off:rotation"]; !ok {
		t.Error("an admin was not offered the switch")
	}
	pn, _ = p.page("g1", "adminconfig", true)
	if _, ok := paneltest.IxButtons(pn)[switchPrefix+"off:adminconfig"]; ok {
		t.Error("adminconfig can be switched off from help")
	}
	if _, ok := p.page("g1", "contest", false); ok {
		t.Error("a member can open a switched-off plugin's page")
	}
}

// Clicking a guide somebody posted gives the clicker a private copy, never
// changes the posted one under everybody else.
func TestAPostedGuideAnswersPrivately(t *testing.T) {
	p, _, _ := setup(t)
	ix, wire := paneltest.IxPublicClick(t, member, viewPrefix+"page", nil, "rotation")
	p.handleView(context.Background(), ix, viewPrefix+"page")
	if !strings.Contains(wire.All(), `"type":4`) || !strings.Contains(wire.All(), `"flags":32832`) {
		t.Errorf("posted guide was not answered with a private copy:\n%.300s", wire.All())
	}
	ix, wire = paneltest.IxClick(t, member, viewPrefix+"index", nil)
	p.handleView(context.Background(), ix, viewPrefix+"index")
	if !strings.Contains(wire.All(), `"type":7`) {
		t.Errorf("a private guide was not turned in place:\n%.300s", wire.All())
	}
}

func TestSwitchIsAuditedAndGuarded(t *testing.T) {
	p, tg, au := setup(t)
	for _, id := range []string{"off:rotation", "on:contest", "off:adminconfig", "off:help", "off:nonesuch"} {
		ix, wire := paneltest.IxClick(t, boss, switchPrefix+id, nil)
		p.handleSwitch(context.Background(), ix, switchPrefix+id)
		if !wire.Answered() {
			t.Errorf("%s left unanswered", id)
		}
	}
	if got := strings.Join(tg.calls, ","); got != "off rotation,on contest" {
		t.Errorf("switch calls = %s", got)
	}
	if got := strings.Join(au.actions, ","); got != "config.plugin_disabled,config.plugin_enabled" {
		t.Errorf("audit = %s", got)
	}
}

func TestHelpCommand(t *testing.T) {
	p, _, _ := setup(t)
	post := func(p map[string]any) {
		p["data"].(map[string]any)["options"] = []any{map[string]any{"name": "post", "type": 5, "value": true}}
	}
	ix, wire := paneltest.IxCommand(t, member, "help", post)
	p.handleHelp(context.Background(), ix)
	if !strings.Contains(wire.All(), "only this server") {
		t.Error("a member posted the guide")
	}
	ix, wire = paneltest.IxCommand(t, boss, "help", post)
	p.handleHelp(context.Background(), ix)
	if !strings.Contains(wire.All(), `"flags":32768`) || strings.Contains(wire.All(), `"flags":32832`) {
		t.Errorf("a posted guide must not be ephemeral:\n%.300s", wire.All())
	}
	ix, wire = paneltest.IxCommand(t, member, "help", nil)
	p.handleHelp(context.Background(), ix)
	if !strings.Contains(wire.All(), `"flags":32832`) {
		t.Errorf("/help on its own was not private:\n%.300s", wire.All())
	}
	if p.Help().Title != "Help" {
		t.Error("help has no page of its own")
	}
}
