package roles

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
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

// sentDM is one direct message the bot tried to deliver.
type sentDM struct {
	channelID string
	data      *discordgo.MessageSend
}

type fakeOps struct {
	mu sync.Mutex

	members map[string]*discordgo.Member // guildID+":"+userID -> member
	roles   map[string][]*discordgo.Role // guildID -> guild roles
	channel map[string]*discordgo.Channel

	nextRoleID int
	overwrites map[overwriteKey]struct{ allow, deny int64 }

	// memberFetchErr, when set, fails every GuildMember call with it, for
	// tests that care about how a *kind* of failure is handled rather than
	// which call fails.
	memberFetchErr error
	// memberEditErr does the same for GuildMemberEdit.
	memberEditErr error
	// memberListErr does the same for GuildMembers, standing in for a guild
	// whose member list can't be paged (most realistically, the GUILD_MEMBERS
	// intent not being granted).
	memberListErr   error
	memberListCalls int

	// guildErr and dmErr stand in for a member with DMs closed or a guild
	// lookup that fails, both of which must leave the jail itself intact.
	guildErr error
	dmErr    error
	dmSends  []sentDM

	roleAddCalls    []string // "guildID:userID:roleID"
	roleRemoveCalls []string
	memberEditCalls map[string][]string // userID -> Roles set on each GuildMemberEdit call

	// voiceKickCalls records every disconnectFromVoice call (the
	// ChannelID-only GuildMemberEdit), by userID, kept separate from
	// memberEditCalls since it's a distinct API call carrying no Roles.
	// voiceKickErr, when set, fails only that call, independent of
	// memberEditErr, so a test can simulate a failing kick without also
	// failing the role strip that precedes it.
	voiceKickCalls []string
	voiceKickErr   error

	// guildChannelsErr, when set, fails every GuildChannels call, standing in
	// for a Discord outage or missing permission mid-fan-out.
	guildChannelsErr error
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

// unknownMemberErr mirrors what discordgo returns for a member who has left
// the guild: a *discordgo.RESTError carrying Discord's own 10007 code.
// Callers distinguish that from a transient failure (core.IsUnknownResource)
// before deciding to stop tracking a jail or grant, so an undifferentiated
// error here would make "they left" and "Discord hiccuped" test identically.
func unknownMemberErr(guildID, userID string) error {
	return &discordgo.RESTError{
		Response:     &http.Response{StatusCode: http.StatusNotFound},
		ResponseBody: []byte(`{"code":10007,"message":"Unknown Member"}`),
		Message: &discordgo.APIErrorMessage{
			Code:    discordgo.ErrCodeUnknownMember,
			Message: fmt.Sprintf("Unknown Member: %s in %s", userID, guildID),
		},
	}
}

// transientErr is a retryable Discord failure (a 500), the counterpart to
// unknownMemberErr.
func transientErr() error {
	return &discordgo.RESTError{
		Response:     &http.Response{StatusCode: http.StatusInternalServerError},
		ResponseBody: []byte(`{"code":0,"message":"500: Internal Server Error"}`),
		Message:      &discordgo.APIErrorMessage{Message: "500: Internal Server Error"},
	}
}

func (f *fakeOps) setMember(guildID, userID string, roleIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[memberKey(guildID, userID)] = &discordgo.Member{User: &discordgo.User{ID: userID}, GuildID: guildID, Roles: roleIDs}
}

// setMemberJoined is setMember plus a JoinedAt, which is what distinguishes a
// member who left and came back from one who never left. See
// rejoinedSinceJail.
func (f *fakeOps) setMemberJoined(guildID, userID string, roleIDs []string, joinedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[memberKey(guildID, userID)] = &discordgo.Member{
		User:     &discordgo.User{ID: userID},
		GuildID:  guildID,
		Roles:    roleIDs,
		JoinedAt: joinedAt,
	}
}

func memberWithJoinedAt(joinedAt time.Time) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: "u1"}, JoinedAt: joinedAt}
}

func (f *fakeOps) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberFetchErr != nil {
		return nil, f.memberFetchErr
	}
	m, ok := f.members[memberKey(guildID, userID)]
	if !ok {
		return nil, unknownMemberErr(guildID, userID)
	}
	cp := *m
	cp.Roles = append([]string(nil), m.Roles...)
	return &cp, nil
}

// GuildMembers pages this guild's members in a stable (ID-sorted) order, so
// pagination behaviour is deterministic and membersWithRole's "after" cursor
// is actually exercised rather than always fitting in one page.
func (f *fakeOps) GuildMembers(guildID string, after string, limit int, options ...discordgo.RequestOption) ([]*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberListErr != nil {
		return nil, f.memberListErr
	}
	f.memberListCalls++

	var all []*discordgo.Member
	for key, m := range f.members {
		if strings.HasPrefix(key, guildID+":") {
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].User.ID < all[j].User.ID })

	var page []*discordgo.Member
	for _, m := range all {
		if after != "" && m.User.ID <= after {
			continue
		}
		if len(page) == limit {
			break
		}
		page = append(page, m)
	}
	return page, nil
}

func (f *fakeOps) GuildMemberEdit(guildID, userID string, data *discordgo.GuildMemberParams, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	if data.ChannelID != nil {
		// The voice-disconnect call: a separate GuildMemberEdit carrying only
		// ChannelID, never combined with a Roles change (see
		// Plugin.disconnectFromVoice).
		f.mu.Lock()
		f.voiceKickCalls = append(f.voiceKickCalls, userID)
		f.mu.Unlock()
		return nil, f.voiceKickErr
	}

	if f.memberEditErr != nil {
		return nil, f.memberEditErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[memberKey(guildID, userID)]
	if !ok {
		return nil, unknownMemberErr(guildID, userID)
	}
	if data.Roles != nil {
		m.Roles = append([]string(nil), (*data.Roles)...)
		f.memberEditCalls[userID] = append(f.memberEditCalls[userID], append([]string(nil), *data.Roles...)...)
	}
	cp := *m
	return &cp, nil
}

// The DM path: a fake guild name, an in-memory DM channel, and a record of
// every notice sent, so a test can assert on what a jailed member is
// actually told rather than only that something was attempted.
func (f *fakeOps) Guild(guildID string, options ...discordgo.RequestOption) (*discordgo.Guild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.guildErr != nil {
		return nil, f.guildErr
	}
	return &discordgo.Guild{ID: guildID, Name: "The Melting Pot"}, nil
}

func (f *fakeOps) UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmErr != nil {
		return nil, f.dmErr
	}
	return &discordgo.Channel{ID: "dm:" + recipientID, Type: discordgo.ChannelTypeDM}, nil
}

func (f *fakeOps) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dmSends = append(f.dmSends, sentDM{channelID: channelID, data: data})
	return &discordgo.Message{ID: "m1"}, nil
}

func (f *fakeOps) GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*discordgo.Role(nil), f.roles[guildID]...), nil
}

// deleteRole removes a role from the guild, standing in for a mod deleting
// it in Discord. Needed so a test can distinguish "the cache was dropped"
// from "the role is actually gone": re-resolving finds the role by name, so
// dropping the cache alone still lands on the same ID.
func (f *fakeOps) deleteRole(guildID, roleID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[guildID] = slices.DeleteFunc(f.roles[guildID], func(r *discordgo.Role) bool {
		return r.ID == roleID
	})
}

func (f *fakeOps) GuildRoleCreate(guildID string, data *discordgo.RoleParams, options ...discordgo.RequestOption) (*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRoleID++
	r := &discordgo.Role{ID: fmt.Sprintf("role-created-%d", f.nextRoleID), Name: data.Name}
	f.roles[guildID] = append(f.roles[guildID], r)
	return r, nil
}

func (f *fakeOps) GuildRoleEdit(guildID, roleID string, data *discordgo.RoleParams, options ...discordgo.RequestOption) (*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.roles[guildID] {
		if r.ID == roleID {
			if data.Permissions != nil {
				r.Permissions = *data.Permissions
			}
			// RoleParams uses *bool for Hoist/Mentionable; if nil leave as-is
			if data.Hoist != nil {
				r.Hoist = *data.Hoist
			}
			if data.Mentionable != nil {
				r.Mentionable = *data.Mentionable
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("fakeOps: role %s not found in guild %s", roleID, guildID)
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
	if f.guildChannelsErr != nil {
		return nil, f.guildChannelsErr
	}
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
	mu sync.Mutex
	// insertJailErr, when set, fails every InsertJail, for testing what the
	// jail mutation leaves behind when the record can't be written.
	insertJailErr error
	jails         map[string]JailRecord  // guildID+":"+userID
	grants        map[string]GrantRecord // guildID+":"+userID+":"+roleID
	nextID        int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{jails: make(map[string]JailRecord), grants: make(map[string]GrantRecord)}
}

func jailKey(guildID, userID string) string          { return guildID + ":" + userID }
func grantKey(guildID, userID, roleID string) string { return guildID + ":" + userID + ":" + roleID }

func (f *fakeStore) InsertJail(ctx context.Context, rec JailRecord) error {
	if f.insertJailErr != nil {
		return f.insertJailErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirrors the real store's ON CONFLICT DO NOTHING: an existing jail is
	// never overwritten, so a lost race can't destroy the role snapshot the
	// winning call recorded.
	key := jailKey(rec.GuildID, rec.UserID)
	if _, exists := f.jails[key]; exists {
		return fmt.Errorf("fake store: insert jail for %s: %w", rec.UserID, ErrAlreadyJailed)
	}
	f.jails[key] = rec
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

// ActiveJails mirrors the real store's "still in force" predicate: not yet
// due, or indefinite. Sorted by user ID rather than the real store's
// jailed_at DESC purely so test output is stable; the ordering only exists
// there to decide what falls off the LIMIT, which this fake has no need for.
func (f *fakeStore) ActiveJails(ctx context.Context, guildID string, now time.Time) ([]JailRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []JailRecord
	for _, rec := range f.jails {
		if rec.GuildID == guildID && (rec.ReleaseAt == nil || rec.ReleaseAt.After(now)) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
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
	mu         sync.Mutex
	allowed    map[string][]string // guildID -> channel IDs
	markerRole map[string]string   // guildID -> configured jail marker role ID
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{allowed: make(map[string][]string), markerRole: make(map[string]string)}
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

// Jail marker role helpers (new API)
func (f *fakeSettings) JailMarkerRoleID(guildID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markerRole[guildID]
}

func (f *fakeSettings) SetJailMarkerRole(ctx context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markerRole[guildID] = roleID
	return nil
}

func (f *fakeSettings) ClearJailMarkerRole(ctx context.Context, guildID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.markerRole, guildID)
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
	// protected are user IDs CanModerate refuses to act on, standing in for
	// core.Permissions' "target is an admin and you aren't" answer without
	// needing a live guild state cache to derive it from.
	protected map[string]bool
	// moderateErr, when set, fails every CanModerate call with it, for the
	// fail-closed case where the guild's state can't be resolved at all.
	moderateErr error
}

func newFakePerms() *fakePerms {
	return &fakePerms{unmanageable: make(map[string]bool), protected: make(map[string]bool)}
}

func (f *fakePerms) CanManageRole(guildID, targetRoleID string) error {
	if f.unmanageable[targetRoleID] {
		return core.ErrForbidden{Reason: "target role at/above bot's top role"}
	}
	return nil
}

func (f *fakePerms) CanModerate(guildID string, actor *discordgo.Member, targetUserID string, targetRoleIDs []string) error {
	if f.moderateErr != nil {
		return f.moderateErr
	}
	if f.protected[targetUserID] {
		return core.ErrForbidden{Reason: "target is an admin, only another admin can do that"}
	}
	return nil
}
