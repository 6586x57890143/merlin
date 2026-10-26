package roles

import (
	"context"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/discord"
	"slices"
)

func recordOwnedOverwrite(p *Plugin, ops *fakeOps, channel, user string) {
	p.botID = core.ID("7000130")
	target := core.ID(channel)
	ops.auditLog = append(ops.auditLog, discord.AuditLogEntry{
		ID: core.ID("8000001"), UserID: p.botID, TargetID: &target,
		ActionType: discord.AuditLogEventChannelOverwriteCreate,
		Options:    &discord.OptionalAuditLogEntryInfo{ID: &user},
	})
}

func (f *fakeSettings) JailConversationChannelIDs(g string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.conversation[g]...)
}
func (f *fakeSettings) VacationConversationChannelIDs(g string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.vacationConversation[g]...)
}
func (f *fakeSettings) AddJailConversationChannel(_ context.Context, g, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowed[g] = slices.DeleteFunc(f.allowed[g], func(id string) bool { return id == ch })
	if !slices.Contains(f.conversation[g], ch) {
		f.conversation[g] = append(f.conversation[g], ch)
	}
	return nil
}
func (f *fakeSettings) RemoveJailConversationChannel(_ context.Context, g, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conversation[g] = slices.DeleteFunc(f.conversation[g], func(id string) bool { return id == ch })
	return nil
}
func (f *fakeSettings) AddVacationConversationChannel(_ context.Context, g, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vacationAllowed[g] = slices.DeleteFunc(f.vacationAllowed[g], func(id string) bool { return id == ch })
	if !slices.Contains(f.vacationConversation[g], ch) {
		f.vacationConversation[g] = append(f.vacationConversation[g], ch)
	}
	return nil
}
func (f *fakeSettings) RemoveVacationConversationChannel(_ context.Context, g, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vacationConversation[g] = slices.DeleteFunc(f.vacationConversation[g], func(id string) bool { return id == ch })
	return nil
}
