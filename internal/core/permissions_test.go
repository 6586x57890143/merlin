package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/config"
)

func newTestLoader(t *testing.T, yaml string) *config.Loader {
	t.Helper()
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	l, err := config.NewLoader(path, testLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return l
}

const testGuildYAML = `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["modrole"]
    admin_user_ids: ["adminuser"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
`

func TestAuthorizeLayer1MissingDiscordPermission(t *testing.T) {
	loader := newTestLoader(t, testGuildYAML)
	perms := NewPermissions(nil, loader)

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "g1",
		Member: &discordgo.Member{
			Permissions: 0,
			Roles:       []string{"modrole"},
			User:        &discordgo.User{ID: "u1"},
		},
	}}

	err := perms.Authorize(i, PermCheck{Required: discordgo.PermissionManageChannels, Action: "test"})
	if err == nil {
		t.Fatal("expected layer 1 denial")
	}
}

func TestAuthorizeLayer2NotOnAllowList(t *testing.T) {
	loader := newTestLoader(t, testGuildYAML)
	perms := NewPermissions(nil, loader)

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "g1",
		Member: &discordgo.Member{
			Permissions: discordgo.PermissionManageChannels,
			Roles:       []string{"someotherrole"},
			User:        &discordgo.User{ID: "u1"},
		},
	}}

	err := perms.Authorize(i, PermCheck{Required: discordgo.PermissionManageChannels, Action: "test"})
	if err == nil {
		t.Fatal("expected layer 2 denial: not on allow-list")
	}
}

func TestAuthorizeSuccessViaModRole(t *testing.T) {
	loader := newTestLoader(t, testGuildYAML)
	perms := NewPermissions(nil, loader)

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "g1",
		Member: &discordgo.Member{
			Permissions: discordgo.PermissionManageChannels,
			Roles:       []string{"modrole"},
			User:        &discordgo.User{ID: "u1"},
		},
	}}

	if err := perms.Authorize(i, PermCheck{Required: discordgo.PermissionManageChannels, Action: "test"}); err != nil {
		t.Fatalf("expected authorize to succeed via mod role, got %v", err)
	}
}

func TestAuthorizeSuccessViaAdminUserID(t *testing.T) {
	loader := newTestLoader(t, testGuildYAML)
	perms := NewPermissions(nil, loader)

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "g1",
		Member: &discordgo.Member{
			Permissions: discordgo.PermissionManageChannels,
			Roles:       []string{},
			User:        &discordgo.User{ID: "adminuser"},
		},
	}}

	if err := perms.Authorize(i, PermCheck{Required: discordgo.PermissionManageChannels, Action: "test"}); err != nil {
		t.Fatalf("expected authorize to succeed via admin allow-list, got %v", err)
	}
}

func TestAuthorizeUnknownGuild(t *testing.T) {
	loader := newTestLoader(t, testGuildYAML)
	perms := NewPermissions(nil, loader)

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "unknown-guild",
		Member:  &discordgo.Member{User: &discordgo.User{ID: "u1"}},
	}}

	if err := perms.Authorize(i, PermCheck{Action: "test"}); err == nil {
		t.Fatal("expected error for unconfigured guild")
	}
}

func TestCanManageRoleHierarchy(t *testing.T) {
	session, err := discordgo.New("Bot faketoken")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	session.State.User = &discordgo.User{ID: "bot"}

	guild := &discordgo.Guild{
		ID: "g1",
		Roles: []*discordgo.Role{
			{ID: "low", Position: 1},
			{ID: "bot-role", Position: 5},
			{ID: "high", Position: 10},
		},
	}
	if err := session.State.GuildAdd(guild); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}
	if err := session.State.MemberAdd(&discordgo.Member{
		GuildID: "g1",
		User:    &discordgo.User{ID: "bot"},
		Roles:   []string{"bot-role"},
	}); err != nil {
		t.Fatalf("MemberAdd: %v", err)
	}

	perms := NewPermissions(session, nil)

	if err := perms.CanManageRole("g1", "low"); err != nil {
		t.Fatalf("expected bot to manage role below it, got %v", err)
	}
	if err := perms.CanManageRole("g1", "high"); err == nil {
		t.Fatal("expected denial for role above bot's top role")
	}
	if err := perms.CanManageRole("g1", "bot-role"); err == nil {
		t.Fatal("expected denial for role at same position as bot's top role")
	}
}

func TestRegisterCommandsFailsClosedWithoutDefaultPermissions(t *testing.T) {
	cmds := []*discordgo.ApplicationCommand{
		{Name: "dangerous"}, // no DefaultMemberPermissions set
	}
	// No network call should happen: validation fails before the session
	// is ever used, so a nil session here is safe for this test.
	err := RegisterCommands(nil, "app", "guild", cmds)
	if err == nil {
		t.Fatal("expected RegisterCommands to fail closed on missing DefaultMemberPermissions")
	}
}
