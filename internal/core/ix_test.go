package core_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
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
func event(t *testing.T, p map[string]any) *discordgo.Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var ic discordgo.InteractionCreate
	if err := json.Unmarshal(raw, &ic); err != nil {
		t.Fatal(err)
	}
	return &discordgo.Event{Type: "INTERACTION_CREATE", RawData: raw, Struct: &ic}
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
	s, sessionWire := paneltest.Session()
	rt.HandleEvent(s, event(t, command("hi")))
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
	s, sessionWire := paneltest.Session()
	rt.HandleEvent(s, event(t, command("ban")))
	if ran {
		t.Fatal("an admin handler ran for a member with no permissions")
	}
	if !strings.Contains(wire.All(), "not allowed") || sessionWire.Answered() {
		t.Fatalf("the refusal was not sent through the interaction: %s", wire.All())
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

// wire is what x marshals to, with every false, zero and empty value
// dropped: Discord reads an absent field as its zero value, and the two
// libraries differ only in which zero values they bother to write.
func wireOf(t *testing.T, x any) any {
	t.Helper()
	raw, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return prune(v)
}

func prune(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e = prune(e); e == nil {
				delete(x, k)
			} else {
				x[k] = e
			}
		}
		if len(x) == 0 {
			return nil
		}
	case []any:
		for n := range x {
			x[n] = prune(x[n])
		}
		if len(x) == 0 {
			return nil
		}
	case bool:
		if !x {
			return nil
		}
	case float64:
		if x == 0 {
			return nil
		}
	case string:
		if x == "" {
			return nil
		}
	}
	return v
}

func names[F any](files []F, name func(F) string) []string {
	var out []string
	for _, f := range files {
		out = append(out, name(f))
	}
	return out
}

// A plugin moved onto the disgo builders must look exactly as it did.
func TestDisgoBuildersMatchTheDiscordgoOnes(t *testing.T) {
	inline := true
	for _, c := range []struct {
		name     string
		old, new any
		oldFiles []string
		newFiles []string
	}{
		{"embed", core.NewEmbed(core.ColorSuccess, "t", "d", &discordgo.MessageEmbedField{Name: "n", Value: "v", Inline: true}),
			core.Embed(core.ColorSuccess, "t", "d", discord.EmbedField{Name: "n", Value: "v", Inline: &inline}), nil, nil},
		{"landmark", core.NewLandmarkEmbed(core.ColorInfo, "t", "d"), core.LandmarkEmbed(core.ColorInfo, "t", "d"), nil, nil},
		{"error embed", core.NewEmbed(core.ColorError, "t", "d"), core.Embed(core.ColorError, "t", "d"), nil, nil},
	} {
		if o, n := wireOf(t, c.old), wireOf(t, c.new); !reflect.DeepEqual(o, n) {
			t.Errorf("%s:\n discordgo %v\n disgo     %v", c.name, o, n)
		}
	}
	for _, color := range []int{core.ColorSuccess, core.ColorError, core.ColorWarning, core.ColorInfo, core.ColorPrimary} {
		o := names(core.EmbedFiles(core.NewEmbed(color, "t", "d")), func(f *discordgo.File) string { return f.Name })
		n := names(core.EmbedUploads(core.Embed(color, "t", "d")), func(f *discord.File) string { return f.Name })
		if !slices.Equal(o, n) {
			t.Errorf("colour %x uploads: discordgo %v, disgo %v", color, o, n)
		}
	}

	old := core.Panel{Color: core.ColorSuccess, Title: "settings", Line: "what this is", Note: "✓ saved", Footer: "bye", Body: []discordgo.MessageComponent{
		core.Text("hello"), core.Gap(),
		core.Row(discordgo.Button{Label: "go", Style: discordgo.PrimaryButton, CustomID: "go"}),
	}}
	pn := old
	pn.Body = nil
	pn.Parts = []discord.ContainerSubComponent{
		discord.TextDisplayComponent{Content: "hello"}, discord.SeparatorComponent{},
		discord.ActionRowComponent{Components: []discord.InteractiveComponent{discord.ButtonComponent{Label: "go", Style: discord.ButtonStylePrimary, CustomID: "go"}}},
	}
	if o, n := wireOf(t, old.Components()), wireOf(t, pn.Layout()); !reflect.DeepEqual(o, n) {
		t.Errorf("panel:\n discordgo %v\n disgo     %v", o, n)
	}
	o := names(old.Files(), func(f *discordgo.File) string { return f.Name })
	n := names(pn.Uploads(), func(f *discord.File) string { return f.Name })
	if !slices.Equal(o, n) {
		t.Errorf("panel uploads: discordgo %v, disgo %v", o, n)
	}
}
