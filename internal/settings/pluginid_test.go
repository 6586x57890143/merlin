package settings

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// PluginEnabledID is what the per-message handlers ask, so it must give
// PluginEnabled's answer in every case PluginEnabled distinguishes, and
// cost nothing to ask.
func TestPluginEnabledIDAgreesWithPluginEnabled(t *testing.T) {
	s := New(nil, nil)
	s.DefaultOff("whisper")
	s.cache["1459404984943644747"] = &guildCache{settings: GuildSettings{
		GuildID:         "1459404984943644747",
		DisabledPlugins: []string{"rotation"},
		EnabledPlugins:  []string{"whisper"},
	}}
	for _, guild := range []snowflake.ID{1459404984943644747, 2} {
		for _, plugin := range []string{"rotation", "whisper", "roles"} {
			if got, want := s.PluginEnabledID(guild, plugin), s.PluginEnabled(guild.String(), plugin); got != want {
				t.Errorf("guild %d %s: PluginEnabledID = %v, PluginEnabled = %v", guild, plugin, got, want)
			}
		}
	}
	if n := testing.AllocsPerRun(100, func() { s.PluginEnabledID(2, "whisper") }); n != 0 {
		t.Errorf("PluginEnabledID allocates %v times on a miss", n)
	}
}

// A guild whose settings failed to refresh after a committed write must not
// read as a guild with nothing configured: its last write may have been a
// pause, a plugin switched off or a deny, and zero values say none of those
// happened. Until RetryStale reads it back, writes are paused, every plugin
// but the recovery one is off, and no action policy can be trusted.
func TestAStaleGuildFailsClosed(t *testing.T) {
	const g = "1459404984943644747"
	s := New(nil, nil)
	s.Recovery("adminconfig")
	s.cache[g] = &guildCache{settings: GuildSettings{GuildID: g}}
	if s.WritesPaused(g) || !s.PluginEnabled(g, "roles") || s.ActionPolicy(g, "roles.jail").Unknown {
		t.Fatal("a healthy guild read as stale")
	}

	s.invalidate(g) // what a mutator does when its refresh fails

	if !s.WritesPaused(g) {
		t.Error("writes are not paused for a guild whose pause state is unknown")
	}
	for _, plugin := range []string{"roles", "rotation", "aimod"} {
		if s.PluginEnabled(g, plugin) || s.PluginEnabledID(1459404984943644747, plugin) {
			t.Errorf("%s is on in a guild whose plugin toggles are unknown", plugin)
		}
	}
	if !s.PluginEnabled(g, "adminconfig") || !s.PluginEnabledID(1459404984943644747, "adminconfig") {
		t.Error("the recovery plugin is off, so nobody can see what is wrong")
	}
	if !s.ActionPolicy(g, "roles.jail").Unknown {
		t.Error("an action policy was answered from settings that cannot be read")
	}
	if n := testing.AllocsPerRun(100, func() { s.PluginEnabledID(1459404984943644747, "roles") }); n != 0 {
		t.Errorf("PluginEnabledID allocates %v times on a stale guild", n)
	}

	s.Forget(g)
	if s.Stale(g) || s.WritesPaused(g) {
		t.Error("a guild the bot left is still treated as stale")
	}
}
