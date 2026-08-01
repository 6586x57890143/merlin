package config

import "testing"

func intPtr(v int) *int { return &v }

func TestValidateGuildRotation(t *testing.T) {
	templates := map[string]StickyTemplate{
		"welcome": {Messages: []string{"hi"}},
	}

	tests := []struct {
		name    string
		gc      GuildConfig
		wantErr bool
	}{
		{
			name: "no rotating channels is fine",
			gc:   GuildConfig{},
		},
		{
			name: "valid finite retention",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only", RetentionDays: intPtr(7)},
				},
			},
		},
		{
			name: "valid forever retention",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only", RetentionDays: nil},
				},
			},
		},
		{
			name: "channel cannot be its own archive category",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "c1", ArchiveVisibility: "mod_only", RetentionDays: intPtr(7)},
				},
			},
			wantErr: true,
		},
		{
			name: "archive category cannot itself be a rotating channel",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "c2", ArchiveVisibility: "mod_only", RetentionDays: intPtr(7)},
					{ChannelID: "c2", IntervalHours: 24, ArchiveCategoryID: "cat2", ArchiveVisibility: "mod_only", RetentionDays: intPtr(7)},
				},
			},
			wantErr: true,
		},
		{
			name: "sticky enabled with unknown template",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{
						ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only", RetentionDays: intPtr(7),
						Sticky: StickyConfig{Enabled: true, Template: "does-not-exist"},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "sticky enabled with known template",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{
						ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only", RetentionDays: intPtr(7),
						Sticky: StickyConfig{Enabled: true, Template: "welcome"},
					},
				},
			},
		},
		{
			name: "whitelist visibility with no whitelist entries",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "whitelist", RetentionDays: intPtr(7)},
				},
			},
			wantErr: true,
		},
		{
			name: "whitelist visibility with a role entry",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{
						ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "whitelist", RetentionDays: intPtr(7),
						ArchiveWhitelistRoleIDs: []string{"role1"},
					},
				},
			},
		},
		{
			name: "whitelist visibility with a user entry",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					{
						ChannelID: "c1", IntervalHours: 24, ArchiveCategoryID: "cat1", ArchiveVisibility: "whitelist", RetentionDays: intPtr(7),
						ArchiveWhitelistUserIDs: []string{"user1"},
					},
				},
			},
		},
		{
			name: "projected accumulation exceeds cap",
			gc: GuildConfig{
				RotatingChannels: []RotationConfig{
					// 1 hour interval, 30 day retention => ~720 concurrently archived channels.
					{ChannelID: "c1", IntervalHours: 1, ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only", RetentionDays: intPtr(30)},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGuildRotation("g1", tt.gc, templates)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateGuildRotation: err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestLoaderRejectsBadRotationConfig(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	invalid := `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["r1"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
    rotating_channels:
      - channel_id: "c1"
        interval_hours: 24
        archive_category_id: "c1"
        archive_visibility: mod_only
        retention_days: 7
`
	path := writeConfig(t, invalid)
	if _, err := NewLoader(path, testLogger()); err == nil {
		t.Fatal("expected loader to reject a rotating channel that is its own archive category")
	}
}

func TestLoaderAcceptsValidRotationConfig(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	valid := `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["r1"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
    rotating_channels:
      - channel_id: "c1"
        interval_hours: 24
        archive_category_id: "cat1"
        archive_visibility: mod_only
        sticky:
          enabled: true
          template: welcome
sticky_templates:
  welcome:
    messages: ["hi there"]
`
	path := writeConfig(t, valid)
	l, err := NewLoader(path, testLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	gc, err := l.Guild("g1")
	if err != nil {
		t.Fatalf("Guild: %v", err)
	}
	if len(gc.RotatingChannels) != 1 {
		t.Fatalf("expected 1 rotating channel, got %d", len(gc.RotatingChannels))
	}
	if gc.RotatingChannels[0].RetentionDays != nil {
		t.Fatalf("expected omitted retention_days to be nil (forever), got %v", *gc.RotatingChannels[0].RetentionDays)
	}
}
