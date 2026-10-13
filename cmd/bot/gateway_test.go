package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
)

// decode is Discord's JSON as disgo's gateway would decode it.
func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// What the rare events' plugins read survives the trip from disgo's types
// to discordgo's: a joiner's names, avatar and join time; an audit entry's
// action, target, actor and timeout change; a role's position; a thread's
// guild, parent and owner.
func TestLegacyKeepsWhatThePluginsRead(t *testing.T) {
	m := legacy[discordgo.Member](decode[discord.Member](t, `{"user":{"id":"4","username":"u","global_name":"Zoe","avatar":"abc"},
		"nick":"zed","roles":["5"],"joined_at":"2026-01-02T03:04:05Z"}`))
	if m == nil || m.User.ID != "4" || m.User.GlobalName != "Zoe" || m.User.Avatar != "abc" || m.Nick != "zed" ||
		!m.JoinedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || len(m.Roles) != 1 || m.Roles[0] != "5" {
		t.Fatalf("member %+v", m)
	}

	e := legacy[discordgo.AuditLogEntry](decode[discord.AuditLogEntry](t, `{"id":"9","action_type":24,"target_id":"4","user_id":"7",
		"reason":"spam","changes":[{"key":"communication_disabled_until","new_value":"2026-01-02T04:00:00Z"}]}`))
	if e == nil || e.ID != "9" || e.ActionType == nil || *e.ActionType != discordgo.AuditLogActionMemberUpdate ||
		e.TargetID != "4" || e.UserID != "7" || e.Reason != "spam" || len(e.Changes) != 1 ||
		*e.Changes[0].Key != discordgo.AuditLogChangeKeyCommunicationDisabledUntil || e.Changes[0].NewValue != "2026-01-02T04:00:00Z" {
		t.Fatalf("entry %+v", e)
	}

	r := legacy[discordgo.Role](decode[discord.Role](t, `{"id":"5","name":"r","position":3,"permissions":"8","managed":true,"color":1}`))
	if r == nil || r.ID != "5" || r.Position != 3 || !r.Managed || r.Permissions != discordgo.PermissionAdministrator {
		t.Fatalf("role %+v", r)
	}

	th := decode[discord.GuildThread](t, `{"id":"8","type":11,"guild_id":"3","parent_id":"2","owner_id":"4","name":"entry",
		"thread_metadata":{"archived":false,"auto_archive_duration":60,"archive_timestamp":"2026-01-02T03:04:05Z","locked":false}}`)
	ch := legacy[discordgo.Channel](th)
	if ch == nil || ch.ID != "8" || ch.GuildID != "3" || ch.ParentID != "2" || ch.OwnerID != "4" || ch.Name != "entry" ||
		ch.Type != discordgo.ChannelTypeGuildPublicThread {
		t.Fatalf("thread %+v", ch)
	}
}
