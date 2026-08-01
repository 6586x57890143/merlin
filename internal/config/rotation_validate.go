package config

import "fmt"

// maxProjectedArchivesPerGuild is a defensive cap on how many concurrently
// archived channels a guild's finite-retention rotation configs may project
// to accumulate, so a misconfiguration (very short interval + very long
// retention) can't silently march a guild toward Discord's 500-channel
// limit. It only applies to channels with a finite RetentionDays — a
// "forever" retention channel has no steady state to project and is a
// deliberate, unbounded escape hatch (see RotationConfig.RetentionDays).
const maxProjectedArchivesPerGuild = 100

// validateGuildRotation performs cross-field checks the struct-tag
// validator can't express: relationships between a guild's rotating
// channels, and references from a guild's config into the global sticky
// template set.
func validateGuildRotation(guildID string, gc GuildConfig, templates map[string]StickyTemplate) error {
	var projectedTotal float64

	for _, rc := range gc.RotatingChannels {
		if rc.ChannelID == rc.ArchiveCategoryID {
			return fmt.Errorf("guild %s: rotating channel %s cannot be its own archive_category_id", guildID, rc.ChannelID)
		}
		if rc.ArchiveVisibility == "whitelist" && len(rc.ArchiveWhitelistRoleIDs) == 0 && len(rc.ArchiveWhitelistUserIDs) == 0 {
			return fmt.Errorf("guild %s: rotating channel %s has archive_visibility \"whitelist\" but no archive_whitelist_role_ids/archive_whitelist_user_ids", guildID, rc.ChannelID)
		}
		for _, other := range gc.RotatingChannels {
			if other.ChannelID == rc.ArchiveCategoryID {
				return fmt.Errorf("guild %s: archive_category_id %s is itself a configured rotating channel", guildID, rc.ArchiveCategoryID)
			}
		}
		if rc.Sticky.Enabled {
			if _, ok := templates[rc.Sticky.Template]; !ok {
				return fmt.Errorf("guild %s: rotating channel %s references unknown sticky template %q", guildID, rc.ChannelID, rc.Sticky.Template)
			}
		}
		if rc.RetentionDays != nil {
			projectedTotal += float64(*rc.RetentionDays) * 24 / float64(rc.IntervalHours)
		}
	}

	if projectedTotal > maxProjectedArchivesPerGuild {
		return fmt.Errorf(
			"guild %s: projected archived-channel count (%.0f) across rotating_channels exceeds the safety cap (%d) — lower retention_days, raise interval_hours, or split across categories",
			guildID, projectedTotal, maxProjectedArchivesPerGuild,
		)
	}
	return nil
}
