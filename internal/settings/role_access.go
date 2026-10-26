package settings

import (
	"context"
	"fmt"
)

func (s *Store) JailConversationChannelIDs(guildID string) []string {
	return s.guild(guildID).settings.JailConversationChannelIDs
}

func (s *Store) VacationConversationChannelIDs(guildID string) []string {
	return s.guild(guildID).settings.VacationConversationChannelIDs
}

func (s *Store) AddJailConversationChannel(ctx context.Context, guildID, channelID string) error {
	return s.setConversationChannel(ctx, guildID, channelID, false, true)
}

func (s *Store) RemoveJailConversationChannel(ctx context.Context, guildID, channelID string) error {
	return s.setConversationChannel(ctx, guildID, channelID, false, false)
}

func (s *Store) AddVacationConversationChannel(ctx context.Context, guildID, channelID string) error {
	return s.setConversationChannel(ctx, guildID, channelID, true, true)
}

func (s *Store) RemoveVacationConversationChannel(ctx context.Context, guildID, channelID string) error {
	return s.setConversationChannel(ctx, guildID, channelID, true, false)
}

// Both columns change atomically: a channel has exactly one access mode.
func (s *Store) setConversationChannel(ctx context.Context, guildID, channelID string, vacation, add bool) error {
	prefix := "jail"
	if vacation {
		prefix = "vacation"
	}
	query := fmt.Sprintf(`UPDATE settings_guild SET %[1]s_conversation_channel_ids = array_remove(%[1]s_conversation_channel_ids, $2), updated_at = now() WHERE guild_id = $1`, prefix)
	if add {
		query = fmt.Sprintf(`INSERT INTO settings_guild (guild_id, %[1]s_conversation_channel_ids, updated_at) VALUES ($1, ARRAY[$2], now())
		ON CONFLICT (guild_id) DO UPDATE SET
		%[1]s_conversation_channel_ids = (SELECT array_agg(DISTINCT r) FROM unnest(settings_guild.%[1]s_conversation_channel_ids || $2) AS r),
		%[1]s_allowed_channel_ids = array_remove(settings_guild.%[1]s_allowed_channel_ids, $2), updated_at = now()`, prefix)
	}
	if _, err := s.pool.Exec(ctx, query, guildID, channelID); err != nil {
		return fmt.Errorf("settings: change conversation channel: %w", err)
	}
	if err := s.Refresh(ctx, guildID); err != nil {
		s.invalidate(guildID)
		return err
	}
	s.publishChanged(ctx, guildID)
	return nil
}
