package adminconfig

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/settings"
)

// fakeREST is the Discord adminconfig talks to as merlin.
type fakeREST struct {
	channel   discord.GuildChannel
	granted   []string
	created   discord.GuildTextChannelCreate
	dmTo      snowflake.ID
	sent      *discord.MessageCreate
	rolesMade int
}

func (f *fakeREST) GetChannel(snowflake.ID, ...rest.RequestOpt) (discord.Channel, error) {
	return f.channel, nil
}

func (f *fakeREST) UpdatePermissionOverwrite(_, overwriteID snowflake.ID, u discord.PermissionOverwriteUpdate, _ ...rest.RequestOpt) error {
	if r, ok := u.(discord.RolePermissionOverwriteUpdate); !ok || *r.Allow != discord.PermissionViewChannel || *r.Deny != 0 {
		panic("not a plain view grant")
	}
	f.granted = append(f.granted, overwriteID.String())
	return nil
}

func (f *fakeREST) CreateGuildChannel(_ snowflake.ID, c discord.GuildChannelCreate, _ ...rest.RequestOpt) (discord.GuildChannel, error) {
	f.created = c.(discord.GuildTextChannelCreate)
	return discord.GuildTextChannel{}, nil
}

func (f *fakeREST) CreateRole(snowflake.ID, discord.RoleCreate, ...rest.RequestOpt) (*discord.Role, error) {
	f.rolesMade++
	return &discord.Role{ID: 400}, nil
}

func (f *fakeREST) CreateDMChannel(userID snowflake.ID, _ ...rest.RequestOpt) (*discord.DMChannel, error) {
	f.dmTo = userID
	var dm discord.DMChannel
	_ = json.Unmarshal([]byte(`{"id":"600","type":1}`), &dm)
	return &dm, nil
}

func (f *fakeREST) CreateMessage(_ snowflake.ID, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.sent = &m
	return &discord.Message{}, nil
}

// withChannels is fakeSettingsAdmin with an audit log channel and mod roles.
type withChannels struct{ fakeSettingsAdmin }

func (withChannels) GuildSettings(string) settings.GuildSettings {
	return settings.GuildSettings{AuditLogChannelID: "300", ModRoleIDs: []string{"201", "202"}}
}

func restPlugin(t *testing.T, st SettingsAdmin) (*Plugin, *fakeREST) {
	t.Helper()
	f := &fakeREST{}
	p := New(st, "x.yaml", nil, nil)
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	p.rest, p.botID, p.auditWriter = f, 2, nopAudit{}
	return p, f
}

// Mods are granted view of the audit log only where they lack it: each grant
// is an entry in the server's own audit log.
func TestModsAreGrantedOnlyWhatTheyLack(t *testing.T) {
	p, f := restPlugin(t, withChannels{})
	f.channel = textChannel(t, `[{"id":"201","type":0,"allow":"1024","deny":"0"}]`)
	p.grantModRolesChannelAccess("100")
	if !slices.Equal(f.granted, []string{"202"}) {
		t.Fatalf("granted %v, want only 202", f.granted)
	}
}

// Setup's channel is created private with merlin able to post in it, and
// its role is created for the mods.
func TestSetupCreatesAPrivateChannelAndTheModRole(t *testing.T) {
	p, f := restPlugin(t, withChannels{})
	f.channel = textChannel(t, `[]`)
	ix, _ := paneltest.IxCommand(t, "11", "config", nil)
	ix.GuildID = "100"
	p.setupChannelCreate(context.Background(), ix, setupStepAuditLog, "Audit log", birdAuditLogChannelName,
		func(context.Context, string, string) error { return nil })
	ows := discord.PermissionOverwrites(f.created.PermissionOverwrites)
	if e, ok := ows.Role(100); !ok || !e.Deny.Has(discord.PermissionViewChannel) {
		t.Fatalf("the channel is not private: %+v", f.created)
	}
	if b, ok := ows.Member(2); !ok || b.Allow != botChannelAllow {
		t.Fatalf("merlin cannot post in her own channel: %+v", f.created)
	}

	ix, _ = paneltest.IxCommand(t, "11", "config", nil)
	ix.GuildID = "100"
	p.handleSetupModRoleCreate(context.Background(), ix, "")
	if f.rolesMade != 1 {
		t.Fatal("no mod role was created")
	}
}

// The onboarding nudge is a DM to the owner that pings nobody.
func TestTheNudgeIsADMThatPingsNobody(t *testing.T) {
	p, f := restPlugin(t, fakeSettingsAdmin{})
	p.NudgeIfUnconfigured(context.Background(), &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "100", OwnerID: "5", Name: "n"}})
	if f.dmTo != 5 || f.sent == nil || f.sent.AllowedMentions == nil || len(f.sent.AllowedMentions.Parse) != 0 || len(f.sent.Embeds) != 1 {
		t.Fatalf("dm to %d: %+v", f.dmTo, f.sent)
	}
}

var _ discordREST = (*fakeREST)(nil)
var _ = core.ColorInfo
