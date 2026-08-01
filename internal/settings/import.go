package settings

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// legacyYAML mirrors the pre-Milestone-4 config.yaml guild-scoped shape.
// It exists only so /config import (internal/plugins/adminconfig) can seed
// settings from an existing config.yaml the first time a deployment
// upgrades to DB-backed settings, or for disaster recovery from a
// version-controlled backup of that file. It is deliberately not shared
// with internal/config's bootstrap loader — import is a rare, explicitly
// human-triggered path, not something the hot startup path needs to know
// about.
type legacyYAML struct {
	Guilds map[string]struct {
		ModRoleIDs        []string `yaml:"mod_role_ids"`
		AdminUserIDs      []string `yaml:"admin_user_ids"`
		AuditLogChannelID string   `yaml:"audit_log_channel_id"`
		StatusChannelID   string   `yaml:"status_channel_id"`
		RotatingChannels  []struct {
			ChannelID               string   `yaml:"channel_id"`
			IntervalHours           int      `yaml:"interval_hours"`
			ArchiveCategoryID       string   `yaml:"archive_category_id"`
			ArchiveVisibility       string   `yaml:"archive_visibility"`
			ArchiveWhitelistRoleIDs []string `yaml:"archive_whitelist_role_ids"`
			ArchiveWhitelistUserIDs []string `yaml:"archive_whitelist_user_ids"`
			RetentionDays           *int     `yaml:"retention_days"`
			Sticky                  struct {
				Enabled  bool     `yaml:"enabled"`
				Messages []string `yaml:"messages"`
			} `yaml:"sticky"`
		} `yaml:"rotating_channels"`
	} `yaml:"guilds"`
}

// ImportFromLegacyYAML reads path (the old config.yaml shape) and writes
// every guild it describes into the settings store via the same mutation
// methods commands use — never runs on its own; only in response to an
// explicit /config import invocation (spec.MD §4a: config changes are
// audited, not silent, and that includes this one, via the caller writing
// an audit-log entry per guild imported).
func (s *Store) ImportFromLegacyYAML(ctx context.Context, path string) (importedGuilds []string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("settings: read legacy config %s: %w", path, err)
	}
	var doc legacyYAML
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("settings: parse legacy config %s: %w", path, err)
	}

	for guildID, gc := range doc.Guilds {
		for _, roleID := range gc.ModRoleIDs {
			if err := s.AddModRole(ctx, guildID, roleID); err != nil {
				return importedGuilds, err
			}
		}
		for _, userID := range gc.AdminUserIDs {
			if err := s.AddAdmin(ctx, guildID, userID); err != nil {
				return importedGuilds, err
			}
		}
		if gc.AuditLogChannelID != "" {
			if err := s.SetAuditLogChannel(ctx, guildID, gc.AuditLogChannelID); err != nil {
				return importedGuilds, err
			}
		}
		if gc.StatusChannelID != "" {
			if err := s.SetStatusChannel(ctx, guildID, gc.StatusChannelID); err != nil {
				return importedGuilds, err
			}
		}
		for _, rc := range gc.RotatingChannels {
			if err := s.UpsertRotationChannel(ctx, RotationChannel{
				GuildID:                 guildID,
				ChannelID:               rc.ChannelID,
				IntervalHours:           rc.IntervalHours,
				ArchiveCategoryID:       rc.ArchiveCategoryID,
				ArchiveVisibility:       rc.ArchiveVisibility,
				ArchiveWhitelistRoleIDs: rc.ArchiveWhitelistRoleIDs,
				ArchiveWhitelistUserIDs: rc.ArchiveWhitelistUserIDs,
				RetentionDays:           rc.RetentionDays,
				StickyEnabled:           rc.Sticky.Enabled,
				StickyMessages:          rc.Sticky.Messages,
			}); err != nil {
				return importedGuilds, err
			}
		}
		importedGuilds = append(importedGuilds, guildID)
	}
	return importedGuilds, nil
}
