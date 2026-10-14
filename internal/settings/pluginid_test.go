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
