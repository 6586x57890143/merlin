package settings

import "testing"

func TestIsConfiguredFalseForEmptyGuild(t *testing.T) {
	gs := GuildSettings{GuildID: "g1"}
	if gs.IsConfigured() {
		t.Fatal("expected a guild with nothing set to be unconfigured")
	}
}

func TestIsConfiguredTrueForEachField(t *testing.T) {
	cases := []struct {
		name string
		gs   GuildSettings
	}{
		{"audit channel", GuildSettings{AuditLogChannelID: "c1"}},
		{"status channel", GuildSettings{StatusChannelID: "c1"}},
		{"mod role", GuildSettings{ModRoleIDs: []string{"r1"}}},
		{"admin", GuildSettings{AdminUserIDs: []string{"u1"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.gs.IsConfigured() {
				t.Fatalf("expected IsConfigured to be true when only %s is set", c.name)
			}
		})
	}
}
