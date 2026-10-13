package core_test

import (
	"reflect"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

// What a discordgo-built message carries over to disgo must be what Discord
// would have been sent anyway, field for field.
func TestConvertedMessagesAreTheSameJSON(t *testing.T) {
	embed := core.NewEmbed(core.ColorWarning, "possible alt", "joined twelve minutes after a ban",
		&discordgo.MessageEmbedField{Name: "account", Value: "<@5>", Inline: true},
		&discordgo.MessageEmbedField{Name: "score", Value: "0.82"})
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "case 41"}
	if o, n := wireOf(t, embed), wireOf(t, core.EmbedOf(embed)); !reflect.DeepEqual(o, n) {
		t.Errorf("embed:\n discordgo %v\n disgo     %v", o, n)
	}

	zero := 0
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Ban", Style: discordgo.DangerButton, CustomID: "rap:ban:5"},
			discordgo.Button{Label: "Dismiss", Style: discordgo.SecondaryButton, CustomID: "rap:dismiss:5", Disabled: true},
			discordgo.Button{Label: "Profile", Style: discordgo.LinkButton, URL: "https://discord.com/users/5"},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.StringSelectMenu, CustomID: "rap:reason", MinValues: &zero, MaxValues: 2,
			Options: []discordgo.SelectMenuOption{{Label: "a", Value: "a", Default: true}, {Label: "b", Value: "b"}},
		}}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.UserSelectMenu, CustomID: "rap:who", MaxValues: 3,
			DefaultValues: []discordgo.SelectMenuDefaultValue{{ID: "7", Type: discordgo.SelectMenuDefaultValueUser}},
		}}},
	}
	if o, n := wireOf(t, components), wireOf(t, core.LayoutOf(components)); !reflect.DeepEqual(o, n) {
		t.Errorf("components:\n discordgo %v\n disgo     %v", o, n)
	}

	if core.LayoutOf(nil) != nil || !reflect.DeepEqual(core.EmbedOf(nil), core.EmbedOf(nil)) {
		t.Error("nil did not convert to nothing")
	}
}

// The member a still-discordgo check reads must be the one who clicked:
// same ID, same roles, same nick, the Administrator bit intact.
func TestMemberGoIsTheInvoker(t *testing.T) {
	ix, _ := paneltest.IxCommand(t, "11", "x", func(p map[string]any) {
		m := p["member"].(map[string]any)
		m["roles"] = []any{"21", "22"}
		m["nick"] = "bird"
		m["permissions"] = "8"
		p["data"].(map[string]any)["resolved"] = map[string]any{"users": map[string]any{
			"31": map[string]any{"id": "31", "username": "target", "global_name": "Target"},
		}}
	})
	m := ix.MemberGo()
	if m == nil || m.User == nil || m.User.ID != "11" || m.Nick != "bird" || !reflect.DeepEqual(m.Roles, []string{"21", "22"}) {
		t.Fatalf("member %+v", m)
	}
	if u := ix.ResolvedUserGo("31"); u == nil || u.ID != "31" || u.Username != "target" || u.GlobalName != "Target" {
		t.Fatalf("resolved %+v", u)
	}
	if ix.ResolvedUserGo("32") != nil {
		t.Fatal("resolved a user Discord did not send")
	}
}
