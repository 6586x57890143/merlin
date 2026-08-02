package roles

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakeOps: in-memory DiscordMemberOps ---

type overwriteKey struct{ channelID, targetID string }

type fakeOps struct {
	mu sync.Mutex

	members map[string]*discordgo.Member // guildID+":"+userID -> member
	roles   map[string][]*discordgo.Role // guildID -> guild roles
	channel map[string]*discordgo.Channel

	nextRoleID int
	overwrites map[overwriteKey]struct{ allow, deny int64 }

	roleAddCalls    []string // "guildID:userID:roleID"
	roleRemoveCalls []string
	memberEditCalls map[string][]string // userID -> Roles set on each GuildMemberEdit call
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		members:         make(map[string]*discordgo.Member),
		roles:           make(map[string][]*discordgo.Role),
		channel:         make(map[string]*discordgo.Channel),
		overwrites:      make(map[overwriteKey]struct{ allow, deny int64 }),
		memberEditCalls: make(map[string][]string),
	}
}

func memberKey(guildID, userID string) string { return guildID + ":" + userID }

func (f *fakeOps) setMember(guildID, userID string, roleIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[memberKey(guildID, userID)] = &discordgo.Member{User: &discordgo.User{ID: userID}, GuildID: guildID, Roles: roleIDs}
}

func (f *fakeOps) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[memberKey(guildID, userID)]
	if !ok {
		return nil, fmt.Errorf("fakeOps: unknown member %s in guild %s", userID, guildID)
	}
	cp := *m
	cp.Roles = append([]string(nil), m.Roles...)
	return &cp, nil
}

func (f *fakeOps) GuildMemberEdit(guildID, userID string, data *discordgo.GuildMemberParams, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[memberKey(guildID, userID)]
	if !ok {
		return nil, fmt.Errorf("fakeOps: unknown member %s in guild %s", userID, guildID)
	}
	if data.Roles != nil {
		m.Roles = append([]string(nil), (*data.Roles)...)
		f.memberEditCalls[userID] = append(f.memberEditCalls[userID], append([]string(nil), *data.Roles...)...)
	}
	cp := *m
	return &cp, nil
}

func (f *fakeOps) GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*discordgo.Role(nil), f.roles[guildID]...), nil
}

func (f *fakeOps) GuildRoleCreate(guildID string, data *discordgo.RoleParams, options ...discordgo.RequestOption) (*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRoleID++
	r := &discordgo.Role{ID: fmt.Sprintf("role-created-%d", f.nextRoleID), Name: data.Name}
	f.roles[guildID] = append(f.roles[guildID], r)
	return r, nil
}

func (f *fakeOps) GuildMemberRoleAdd(guildID, userID, roleID string, options ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roleAddCalls = append(f.roleAddCalls, guildID+":"+userID+":"+roleID)
	if m, ok := f.members[memberKey(guildID, userID)]; ok {
		for _, r := range m.Roles {
			if r == roleID {
				return nil
			}
		}
		m.Roles = append(m.Roles, roleID)
	}
	return nil
}

func (f *fakeOps) GuildMemberRoleRemove(guildID, userID, roleID string, options ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roleRemoveCalls = append(f.roleRemoveCalls, guildID+":"+userID+":"+roleID)
	if m, ok := f.members[memberKey(guildID, userID)]; ok {
		out := m.Roles[:0]
		for _, r := range m.Roles {
			if r != roleID {
				out = append(out, r)
			}
		}
		m.Roles = out
	}
	return nil
}

func (f *fakeOps) Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channel[channelID]
	if !ok {
		return nil, fmt.Errorf("fakeOps: unknown channel %s", channelID)
	}
	return ch, nil
}

func (f *fakeOps) GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*discordgo.Channel
	for _, ch := range f.channel {
		if ch.GuildID == guildID {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (f *fakeOps) ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64, options ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overwrites[overwriteKey{channelID, targetID}] = struct{ allow, deny int64 }{allow, deny}
	return nil
}

func (f *fakeOps) ChannelPermissionDelete(channelID, targetID string, options ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.overwrites, overwriteKey{channelID, targetID})
	return nil
}

// --- fakeStore: in-memory Store ---

type fakeStore struct {
	mu     sync.Mutex
	jails  map[string]JailRecord  // guildID+":"+userID
	grants map[string]GrantRecord // guildID+":"+userID+":"+roleID
	nextID int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{jails: make(map[string]JailRecord), grants: make(map[string]GrantRecord)}
}

func jailKey(guildID, userID string) string        { return guildID + ":" + userID }
func grantKey(guildID, userID, roleID string) string { return guildID + ":" + userID + ":" + roleID }

func (f *fakeStore) InsertJail(ctx context.Context, rec JailRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jails[jailKey(rec.GuildID, rec.UserID)] = rec
	return nil
}

func (f *fakeStore) GetJail(ctx context.Context, guildID, userID string) (JailRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.jails[jailKey(guildID, userID)]
	return rec, ok, nil
}

func (f *fakeStore) DeleteJail(ctx context.Context, guildID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jails, jailKey(guildID, userID))
	return nil
}

func (f *fakeStore) DueJails(ctx context.Context, guildID string, now time.Time) ([]JailRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []JailRecord
	for _, rec := range f.jails {
		if rec.GuildID == guildID && rec.ReleaseAt != nil && !rec.ReleaseAt.After(now) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeStore) InsertGrant(ctx context.Context, rec GrantRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	rec.ID = f.nextID
	f.grants[grantKey(rec.GuildID, rec.UserID, rec.RoleID)] = rec
	return nil
}

func (f *fakeStore) GetGrant(ctx context.Context, guildID, userID, roleID string) (GrantRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.grants[grantKey(guildID, userID, roleID)]
	return rec, ok, nil
}

func (f *fakeStore) DeleteGrant(ctx context.Context, guildID, userID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grants, grantKey(guildID, userID, roleID))
	return nil
}

func (f *fakeStore) DueGrants(ctx context.Context, guildID string, now time.Time) ([]GrantRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []GrantRecord
	for _, rec := range f.grants {
		if rec.GuildID == guildID && rec.ExpiresAt != nil && !rec.ExpiresAt.After(now) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeStore) ListGrants(ctx context.Context, guildID, userID string) ([]GrantRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []GrantRecord
	for _, g := range f.grants {
		if g.GuildID == guildID && g.UserID == userID {
			out = append(out, g)
		}
	}
	return out, nil
}

// --- fakeSettings: in-memory JailChannelConfig ---

type fakeSettings struct {
	mu      sync.Mutex
	allowed map[string][]string // guildID -> channel IDs
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{allowed: make(map[string][]string)}
}

func (f *fakeSettings) JailAllowedChannelIDs(guildID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.allowed[guildID]...)
}

func (f *fakeSettings) AddJailAllowedChannel(ctx context.Context, guildID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.allowed[guildID] {
		if id == channelID {
			return nil
		}
	}
	f.allowed[guildID] = append(f.allowed[guildID], channelID)
	return nil
}

func (f *fakeSettings) RemoveJailAllowedChannel(ctx context.Context, guildID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.allowed[guildID][:0]
	for _, id := range f.allowed[guildID] {
		if id != channelID {
			out = append(out, id)
		}
	}
	f.allowed[guildID] = out
	return nil
}

// --- fakeAudit: in-memory core.AuditWriter ---

type auditRecord struct{ guildID, actorID, action, oldValue, newValue string }

type fakeAudit struct {
	mu      sync.Mutex
	records []auditRecord
}

func newFakeAudit() *fakeAudit { return &fakeAudit{} }

func (f *fakeAudit) Record(ctx context.Context, guildID, actorID, action, oldValue, newValue string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, auditRecord{guildID, actorID, action, oldValue, newValue})
	return nil
}

// --- fakePerms: in-memory RoleManager ---

type fakePerms struct {
	unmanageable map[string]bool // role IDs the "bot" can't manage
}

func newFakePerms() *fakePerms { return &fakePerms{unmanageable: make(map[string]bool)} }

func (f *fakePerms) CanManageRole(guildID, targetRoleID string) error {
	if f.unmanageable[targetRoleID] {
		return core.ErrForbidden{Reason: "target role at/above bot's top role"}
	}
	return nil
}
