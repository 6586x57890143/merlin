package core_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

type openGate struct{}

func (openGate) PluginEnabled(string, string) bool { return true }

type noSettings struct{}

func (noSettings) ModRoleIDs(string) []string                    { return nil }
func (noSettings) AdminUserIDs(string) []string                  { return nil }
func (noSettings) ActionPolicy(string, string) core.ActionPolicy { return core.ActionPolicy{} }

// event is what discordgo hands the router for an interaction: Discord's
// JSON, and discordgo's own decoding of it.
func raw(t *testing.T, p map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func command(name string) map[string]any {
	return map[string]any{
		"id": "1000", "application_id": "2", "type": 2, "token": "t", "version": 1,
		"guild_id": "100", "channel_id": "101", "channel": map[string]any{"id": "101", "type": 0},
		"member": map[string]any{
			"user":        map[string]any{"id": "11", "username": "member"},
			"permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z",
		},
		"data": map[string]any{"id": "6", "name": name, "type": 1},
	}
}

func router() (*core.CommandRouter, *paneltest.Wire) {
	r, w := paneltest.Rest()
	perms := core.NewPermissions(nil, noSettings{}, "")
	return core.NewCommandRouter(perms, openGate{}, slog.New(slog.NewTextHandler(io.Discard, nil))).WithRest(r), w
}

func TestIxHandlerAnswersThroughDisgo(t *testing.T) {
	rt, wire := router()
	var got *core.Ix
	rt.HandleIx("hi", "", core.PermSpec{Tier: core.TierPublic}, func(_ context.Context, ix *core.Ix) {
		got = ix
		ix.RespondOK("done", "it worked")
	})
	_, sessionWire := paneltest.Session()
	rt.HandleRaw(raw(t, command("hi")))
	if got == nil || got.GuildID != "100" || got.UserID != "11" {
		t.Fatalf("handler got %+v", got)
	}
	body := wire.All()
	for _, want := range []string{`"type":4`, `"flags":64`, `"parse":[]`, "merlin_ok.png", "merlin_rule.png", "it worked"} {
		if !strings.Contains(body, want) {
			t.Errorf("response is missing %s:\n%s", want, body)
		}
	}
	if !wire.Answered() || sessionWire.Answered() {
		t.Fatal("answered through the wrong client")
	}
}

func TestIxHandlerIsBehindAuthorize(t *testing.T) {
	rt, wire := router()
	ran := false
	rt.HandleIx("ban", "", core.PermSpec{Tier: core.TierAdmin, Action: "x.ban"}, func(context.Context, *core.Ix) { ran = true })
	_, sessionWire := paneltest.Session()
	rt.HandleRaw(raw(t, command("ban")))
	if ran {
		t.Fatal("an admin handler ran for a member with no permissions")
	}
	if !strings.Contains(wire.All(), "not allowed") || sessionWire.Answered() {
		t.Fatalf("the refusal was not sent through the interaction: %s", wire.All())
	}
}

// Interaction replies go through the client merlin instruments for /perf and
// for cygnet, not through one of disgo's own.
func TestNewRestUsesTheClientItIsGiven(t *testing.T) {
	_, wire := paneltest.Rest()
	r := core.NewRest("test", &http.Client{Transport: wire})
	ix, _ := paneltest.IxCommand(t, "11", "x", nil)
	ix.Rest = r
	if err := ix.RespondText("hi"); err != nil || !wire.Answered() {
		t.Fatalf("the reply did not go through the given client: %v", err)
	}
}

// A payload without a channel decodes rather than panicking: decoding runs
// before any handler's recover.
func TestAnInteractionWithoutAChannelDecodes(t *testing.T) {
	r, _ := paneltest.Rest()
	ix, err := core.NewIx([]byte(`{"id":"1000","application_id":"2","type":2,"token":"t","version":1,"guild_id":"100",
		"member":{"user":{"id":"11","username":"m"},"permissions":"0","roles":[],"joined_at":"2026-01-01T00:00:00Z"},
		"data":{"id":"6","name":"x","type":1}}`), r)
	if err != nil || ix.ChannelID != "" || ix.GuildID != "100" {
		t.Fatalf("ix %+v, err %v", ix, err)
	}
}

// What cannot be decoded cannot be answered either, its token being inside
// it, and must not reach a handler.
func TestUndecodableInteractionsAreDropped(t *testing.T) {
	rt, wire := router()
	ran := false
	rt.HandleIx("hi", "", core.PermSpec{Tier: core.TierPublic}, func(context.Context, *core.Ix) { ran = true })
	rt.HandleRaw([]byte(`{"type":2,"data":"not an object"}`))
	if ran || wire.Answered() {
		t.Fatal("an undecodable interaction reached a handler or was answered")
	}
}

func TestIxReadsSelectsAndForms(t *testing.T) {
	pn := core.Panel{Color: core.ColorInfo, Title: "t", Parts: []discord.ContainerSubComponent{
		discord.ActionRowComponent{Components: []discord.InteractiveComponent{discord.StringSelectMenuComponent{
			CustomID: "pick", Options: []discord.StringSelectMenuOption{{Label: "a", Value: "a", Default: true}, {Label: "b", Value: "b"}},
		}}},
		discord.ActionRowComponent{Components: []discord.InteractiveComponent{discord.RoleSelectMenuComponent{
			CustomID: "roles", DefaultValues: []discord.SelectMenuDefaultValue{{Type: discord.SelectMenuDefaultValueTypeRole, ID: snowflake.ID(5)}},
		}}},
	}}
	ix, _ := paneltest.IxPick(t, "11", "pick", &pn, "b")
	if added, removed := ix.SelectDiff("pick"); !slices.Equal(added, []string{"b"}) || !slices.Equal(removed, []string{"a"}) {
		t.Fatalf("string select diff: +%v -%v", added, removed)
	}
	if ix.CustomID() != "pick" || ix.FirstValue() != "b" {
		t.Fatalf("custom ID %q, first value %q", ix.CustomID(), ix.FirstValue())
	}
	ix, _ = paneltest.IxComponent(t, "11", "roles", 6, &pn, "6")
	if added, removed := ix.SelectDiff("roles"); !slices.Equal(added, []string{"6"}) || !slices.Equal(removed, []string{"5"}) {
		t.Fatalf("role select diff: +%v -%v", added, removed)
	}
	ix, _ = paneltest.IxSubmit(t, "11", "form", map[string]string{"code": "XYZ", "note": ""})
	if v := ix.ModalValues(); v["code"] != "XYZ" || v["note"] != "" || ix.CustomID() != "form" {
		t.Fatalf("modal values %v", v)
	}
}

func names[F any](files []F, name func(F) string) []string {
	var out []string
	for _, f := range files {
		out = append(out, name(f))
	}
	return out
}

// The embed and panel builders draw exactly what the discordgo ones did:
// these are those payloads and uploads, byte for byte.
func TestBuildersDrawWhatTheyAlwaysDrew(t *testing.T) {
	pn := core.Panel{Color: core.ColorSuccess, Title: "settings", Line: "what this is", Note: "✓ saved", Footer: "bye", Parts: []discord.ContainerSubComponent{
		discord.TextDisplayComponent{Content: "hello"}, discord.SeparatorComponent{},
		discord.ActionRowComponent{Components: []discord.InteractiveComponent{discord.ButtonComponent{Label: "go", Style: discord.ButtonStylePrimary, CustomID: "go"}}},
	}}
	for _, c := range []struct {
		name string
		got  any
		want string
	}{
		{"embed", core.Embed(core.ColorSuccess, "t", "d", discord.EmbedField{Name: "n", Value: "v", Inline: core.Inline()}),
			`{"title":"t","description":"d","color":8162138,"image":{"url":"attachment://merlin_rule.png"},"thumbnail":{"url":"attachment://merlin_ok.png"},"fields":[{"name":"n","value":"v","inline":true}]}`},
		{"landmark", core.LandmarkEmbed(core.ColorInfo, "t", "d"),
			`{"title":"t","description":"d","color":8027576,"image":{"url":"attachment://merlin_banner.png"}}`},
		{"error embed", core.Embed(core.ColorError, "t", "d"),
			`{"title":"t","description":"d","color":11031092,"image":{"url":"attachment://merlin_rule.png"},"thumbnail":{"url":"attachment://merlin_error.png"}}`},
		{"panel", pn.Layout(),
			`[{"type":17,"accent_color":8162138,"components":[{"type":9,"components":[{"type":10,"content":"## settings\n-# what this is"}],"accessory":{"type":11,"media":{"url":"attachment://merlin_ok.png","flags":0}}},{"type":12,"items":[{"media":{"url":"attachment://merlin_rule.png","flags":0}}]},{"type":10,"content":"✓ saved"},{"type":10,"content":"hello"},{"type":14},{"type":1,"components":[{"type":2,"style":1,"label":"go","custom_id":"go"}]},{"type":10,"content":"-# bye"}]}]`},
	} {
		raw, err := json.Marshal(c.got)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, raw, c.want)
		}
	}
	name := func(f *discord.File) string { return f.Name }
	if got := names(pn.Uploads(), name); !slices.Equal(got, []string{"merlin_rule.png", "merlin_ok.png"}) {
		t.Errorf("panel uploads %v", got)
	}
	for color, mood := range map[int]string{core.ColorSuccess: "ok", core.ColorError: "error", core.ColorWarning: "warn", core.ColorInfo: "info", core.ColorPrimary: "notice"} {
		if got := names(core.EmbedUploads(core.Embed(color, "t", "d")), name); !slices.Equal(got, []string{"merlin_" + mood + ".png", "merlin_rule.png"}) {
			t.Errorf("colour %x uploads %v", color, got)
		}
	}
}
