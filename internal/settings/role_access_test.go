package settings

import (
	"context"
	"slices"
	"testing"
)

func TestRoleChannelModesRoundTripAndRemainExclusive(t *testing.T) {
	s, _, guild := setupStore(t)
	ctx := context.Background()
	for _, vacation := range []bool{false, true} {
		readOnly, conversation := s.JailAllowedChannelIDs, s.JailConversationChannelIDs
		addReadOnly, addConversation, remove := s.AddJailAllowedChannel, s.AddJailConversationChannel, s.RemoveJailConversationChannel
		if vacation {
			readOnly, conversation = s.VacationAllowedChannelIDs, s.VacationConversationChannelIDs
			addReadOnly, addConversation, remove = s.AddVacationAllowedChannel, s.AddVacationConversationChannel, s.RemoveVacationConversationChannel
		}
		if err := addReadOnly(ctx, guild, "channel"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(readOnly(guild), []string{"channel"}) || len(conversation(guild)) != 0 {
			t.Fatal("read-only did not persist")
		}
		for range 2 {
			if err := addConversation(ctx, guild, "channel"); err != nil {
				t.Fatal(err)
			}
		}
		if len(readOnly(guild)) != 0 || !slices.Equal(conversation(guild), []string{"channel"}) {
			t.Fatal("mode switch must be exclusive and idempotent")
		}
		if err := addReadOnly(ctx, guild, "channel"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(readOnly(guild), []string{"channel"}) || len(conversation(guild)) != 0 {
			t.Fatal("switch back did not clear conversation")
		}
		if err := addConversation(ctx, guild, "channel"); err != nil {
			t.Fatal(err)
		}
		if err := remove(ctx, guild, "channel"); err != nil {
			t.Fatal(err)
		}
		if len(readOnly(guild))+len(conversation(guild)) != 0 {
			t.Fatal("channel removal did not persist")
		}
	}
}
