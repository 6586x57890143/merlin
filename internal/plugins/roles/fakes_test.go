package roles

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordtest"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakeOps: in-memory DiscordMemberOps ---

type overwriteKey struct{ channelID, targetID string }

type permPair struct{ allow, deny discord.Permissions }

// sentDM is one direct message the bot tried to deliver.
type sentDM struct {
	channelID string
	data      discord.MessageCreate
}

// fch and fow are the shared channel fixtures.
type (
	fch = discordtest.Channel
	fow = discordtest.Overwrite
)

// fuser, fmember and frole are a user, a member and a role as the fake
// holds them: string IDs, as the tests and merlin's stores spell them.
type fuser struct {
	ID, Username string
	Bot          bool
}

type fmember struct {
	User     *fuser
	GuildID  string
	Roles    []string
	JoinedAt time.Time
}

func (m *fmember) disgo() *discord.Member {
	out := &discord.Member{User: discord.User{ID: core.ID(m.User.ID), Username: m.User.Username, Bot: m.User.Bot},
		GuildID: core.ID(m.GuildID), RoleIDs: core.IDs(m.Roles)}
	if !m.JoinedAt.IsZero() {
		at := m.JoinedAt
		out.JoinedAt = &at
	}
	return out
}

type frole struct {
	ID, Name, UnicodeEmoji, Icon string
	Color, Position              int
	Hoist, Mentionable, Managed  bool
	Permissions                  discord.Permissions
}

func (r *frole) disgo() discord.Role {
	out := discord.Role{ID: core.ID(r.ID), Name: r.Name, Color: r.Color, Position: r.Position,
		Hoist: r.Hoist, Mentionable: r.Mentionable, Managed: r.Managed, Permissions: r.Permissions}
	if r.UnicodeEmoji != "" {
		e := r.UnicodeEmoji
		out.Emoji = &e
	}
	if r.Icon != "" {
		i := r.Icon
		out.Icon = &i
	}
	return out
}

type fakeOps struct {
	mu sync.Mutex

	members map[string]*fmember // guildID+":"+userID -> member
	roles   map[string][]*frole // guildID -> guild roles
	channel map[string]*fch

	nextRoleID int
	overwrites map[overwriteKey]permPair
	// roleCreateErr fails the next CreateRole once, standing in for a guild
	// refusing a role icon; reorders records every UpdateRolePositions.
	roleCreateErr bool
	reorders      [][]discord.RolePositionUpdate

	// memberFetchErr, when set, fails every GetMember call with it, for
	// tests that care about how a *kind* of failure is handled rather than
	// which call fails.
	memberFetchErr error
	// memberFetchErrFor fails GetMember for one user only, so a sweep can
	// be shown carrying on past a single bad row.
	memberFetchErrFor map[string]error
	// memberEditErr does the same for UpdateMember.
	memberEditErr error
	// onMemberEdit, when set, runs after a role edit lands with the roles
	// just set, standing in for the GUILD_MEMBER_UPDATE Discord dispatches
	// for the bot's own edit, which arrives while the caller is still
	// mid-mutation.
	onMemberEdit func(guildID, userID string, roles []string)
	// memberListErr does the same for GetMembers, standing in for a guild
	// whose member list can't be paged (most realistically, the GUILD_MEMBERS
	// intent not being granted).
	memberListErr   error
	memberListCalls int

	// guildErr and dmErr stand in for a member with DMs closed or a guild
	// lookup that fails, both of which must leave the jail itself intact.
	guildErr error
	// guildOwner is the guild's OwnerID, for the owner-or-operator gate.
	guildOwner string
	dmErr      error
	dmSends    []sentDM

	roleAddCalls    []string // "guildID:userID:roleID"
	roleRemoveCalls []string
	memberEditCalls map[string][]string // userID -> Roles set on each UpdateMember call

	// voiceKickCalls records every disconnectFromVoice call by userID, kept
	// separate from memberEditCalls since it's a distinct API call carrying
	// no roles. voiceKickErr, when set, fails only that call, independent of
	// memberEditErr, so a test can simulate a failing kick without also
	// failing the role strip that precedes it.
	voiceKickCalls []string
	voiceKickErr   error

	// guildChannelsErr, when set, fails every GetGuildChannels call, standing
	// in for a Discord outage or missing permission mid-fan-out.
	guildChannelsErr error

	// auditLog is what GetAuditLog answers (filtered by action type);
	// auditLogErr fails it, standing in for a missing View Audit Log.
	auditLog    []discord.AuditLogEntry
	auditLogErr error

	// permSetCalls/permDeleteCalls count permission writes, which is what a
	// guild's Discord audit log gets one entry per: the no-op-skip tests
	// assert on these, not just on the resulting overwrites.
	permSetCalls    int
	permDeleteCalls int

	// knownUsers are the accounts GET /users/{id} answers for, which is the
	// only thing that separates a real account who is simply somewhere else
	// from a mistyped snowflake. Nothing in members implies a user here:
	// resolveTargets asks this exactly when the member fetch already said
	// Unknown Member, so a test that wants an absent-but-real account
	// registers the user without a member.
	knownUsers map[string]bool
	userCalls  []string
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		members:         make(map[string]*fmember),
		roles:           make(map[string][]*frole),
		channel:         make(map[string]*fch),
		overwrites:      make(map[overwriteKey]permPair),
		memberEditCalls: make(map[string][]string),
		knownUsers:      make(map[string]bool),
	}
}

// setUser registers userID as a real Discord account, with no membership of
// any guild.
func (f *fakeOps) setUser(userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.knownUsers[userID] = true
}

func restErr(status, code int, msg string) error {
	return &rest.Error{Code: rest.JSONErrorCode(code), Message: msg, Response: &http.Response{StatusCode: status}}
}

func (f *fakeOps) GetUser(userID snowflake.ID) (*discord.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := userID.String()
	f.userCalls = append(f.userCalls, id)
	if !f.knownUsers[id] {
		return nil, restErr(http.StatusNotFound, 10013, "Unknown User")
	}
	return &discord.User{ID: userID, Username: "absent-" + id}, nil
}

func memberKey(guildID, userID string) string { return guildID + ":" + userID }

// unknownMemberErr is what Discord answers for a member who has left the
// guild: its own 10007 code. Callers distinguish that from a transient
// failure (core.IsUnknownResource) before deciding to stop tracking a jail
// or grant, so an undifferentiated error here would make "they left" and
// "Discord hiccuped" test identically.
func unknownMemberErr(guildID, userID string) error {
	return restErr(http.StatusNotFound, 10007, fmt.Sprintf("Unknown Member: %s in %s", userID, guildID))
}

// transientErr is a retryable Discord failure (a 500), the counterpart to
// unknownMemberErr.
func transientErr() error {
	return restErr(http.StatusInternalServerError, 0, "500: Internal Server Error")
}

func (f *fakeOps) setMember(guildID, userID string, roleIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[memberKey(guildID, userID)] = &fmember{User: &fuser{ID: userID}, GuildID: guildID, Roles: roleIDs}
}

// setMemberJoined is setMember plus a JoinedAt, which is what distinguishes a
// member who left and came back from one who never left. See
// rejoinedSinceJail.
func (f *fakeOps) setMemberJoined(guildID, userID string, roleIDs []string, joinedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[memberKey(guildID, userID)] = &fmember{
		User:     &fuser{ID: userID},
		GuildID:  guildID,
		Roles:    roleIDs,
		JoinedAt: joinedAt,
	}
}

func memberWithJoinedAt(joinedAt time.Time) *discord.Member {
	return (&fmember{User: &fuser{ID: "7000102"}, JoinedAt: joinedAt}).disgo()
}

// GuildMember is the test's own read of a member: a copy, never failed.
func (f *fakeOps) GuildMember(guildID, userID string) (*fmember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[memberKey(guildID, userID)]
	if !ok {
		return nil, unknownMemberErr(guildID, userID)
	}
	cp := *m
	cp.Roles = append([]string(nil), m.Roles...)
	return &cp, nil
}

// disgoRoles is roles as GetRoles hands them out.
func disgoRoles(roles []*frole) []discord.Role {
	out := make([]discord.Role, len(roles))
	for i, r := range roles {
		out[i] = r.disgo()
	}
	return out
}

func (f *fakeOps) GetMember(guildID, userID snowflake.ID) (*discord.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberFetchErr != nil {
		return nil, f.memberFetchErr
	}
	if err := f.memberFetchErrFor[userID.String()]; err != nil {
		return nil, err
	}
	m, ok := f.members[memberKey(guildID.String(), userID.String())]
	if !ok {
		return nil, unknownMemberErr(guildID.String(), userID.String())
	}
	return m.disgo(), nil
}

// GetMembers pages this guild's members in a stable (ID-sorted) order, so
// pagination behaviour is deterministic and membersWithRole's "after" cursor
// is actually exercised rather than always fitting in one page.
func (f *fakeOps) GetMembers(guildID snowflake.ID, limit int, after snowflake.ID) ([]discord.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberListErr != nil {
		return nil, f.memberListErr
	}
	f.memberListCalls++

	var all []*fmember
	for key, m := range f.members {
		if strings.HasPrefix(key, guildID.String()+":") {
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool { return core.ID(all[i].User.ID) < core.ID(all[j].User.ID) })

	var page []discord.Member
	for _, m := range all {
		if after != 0 && core.ID(m.User.ID) <= after {
			continue
		}
		if len(page) == limit {
			break
		}
		page = append(page, *m.disgo())
	}
	return page, nil
}

func (f *fakeOps) UpdateMember(guildID, userID snowflake.ID, data discord.MemberUpdate) (*discord.Member, error) {
	if f.memberEditErr != nil {
		return nil, f.memberEditErr
	}
	g, u := guildID.String(), userID.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[memberKey(g, u)]
	if !ok {
		return nil, unknownMemberErr(g, u)
	}
	var roles []string
	if data.Roles != nil {
		roles = core.IDStrings(*data.Roles)
		m.Roles = append([]string(nil), roles...)
		f.memberEditCalls[u] = append(f.memberEditCalls[u], roles...)
	}
	out := m.disgo()
	if data.Roles != nil && f.onMemberEdit != nil {
		f.mu.Unlock()
		f.onMemberEdit(g, u, roles)
		f.mu.Lock()
	}
	return out, nil
}

// DisconnectMember is the voice disconnect: its own call, never combined
// with a role change (see Plugin.disconnectFromVoice).
func (f *fakeOps) DisconnectMember(_, userID snowflake.ID) error {
	f.mu.Lock()
	f.voiceKickCalls = append(f.voiceKickCalls, userID.String())
	f.mu.Unlock()
	return f.voiceKickErr
}

// The DM path: a fake guild name, an in-memory DM channel, and a record of
// every notice sent, so a test can assert on what a jailed member is
// actually told rather than only that something was attempted.
func (f *fakeOps) GetGuild(guildID snowflake.ID) (*discord.RestGuild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.guildErr != nil {
		return nil, f.guildErr
	}
	g := &discord.RestGuild{}
	g.ID, g.Name, g.OwnerID = guildID, "The Melting Pot", core.ID(f.guildOwner)
	return g, nil
}

func (f *fakeOps) CreateDMChannel(recipientID snowflake.ID) (*discord.DMChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmErr != nil {
		return nil, f.dmErr
	}
	return discordtest.DM(dmWith(recipientID.String())), nil
}

// dmWith is the DM channel the fake opens with userID.
func dmWith(userID string) string { return "9" + userID }

func (f *fakeOps) CreateMessage(channelID snowflake.ID, data discord.MessageCreate) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dmSends = append(f.dmSends, sentDM{channelID: channelID.String(), data: data})
	return &discord.Message{ID: 9001}, nil
}

// everyonePerms is what the fake guild's @everyone role carries: a plain
// member can see, talk, react and join voice, the ordinary Discord default.
// memberBaseline caps a marker role against this, so a fixture that never
// set up an @everyone role would otherwise withhold everything.
const everyonePerms = discord.PermissionViewChannel | discord.PermissionSendMessages |
	discord.PermissionReadMessageHistory | discord.PermissionAddReactions |
	discord.PermissionConnect | discord.PermissionSpeak

func (f *fakeOps) GetRoles(guildID snowflake.ID) ([]discord.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := guildID.String()
	var out []discord.Role
	for _, r := range f.roles[g] {
		out = append(out, r.disgo())
	}
	// Every real guild has its @everyone role, ID equal to the guild's.
	// Fixtures list only the roles they care about, so it is synthesised
	// here unless a test set its own (to lock the guild down, say).
	if !slices.ContainsFunc(out, func(r discord.Role) bool { return r.ID == guildID }) {
		out = append(out, discord.Role{ID: guildID, Name: "@everyone", Permissions: everyonePerms})
	}
	return out, nil
}

// deleteRole removes a role from the guild, standing in for a mod deleting
// it in Discord. Needed so a test can distinguish "the cache was dropped"
// from "the role is actually gone": re-resolving finds the role by name, so
// dropping the cache alone still lands on the same ID.
func (f *fakeOps) deleteRole(guildID, roleID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[guildID] = slices.DeleteFunc(f.roles[guildID], func(r *frole) bool {
		return r.ID == roleID
	})
}

func (f *fakeOps) CreateRole(guildID snowflake.ID, data discord.RoleCreate) (*discord.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roleCreateErr {
		f.roleCreateErr = false
		return nil, fmt.Errorf("fakeOps: role create refused")
	}
	f.nextRoleID++
	// Everything the eternal-role script recreates from, so a test can
	// check the copy was faithful. Icon becomes a hash the way Discord
	// answers an upload with one.
	r := &frole{ID: strconv.Itoa(7900000 + f.nextRoleID), Name: data.Name, Color: data.Color,
		Hoist: data.Hoist, Mentionable: data.Mentionable, UnicodeEmoji: data.Emoji}
	if data.Permissions != nil {
		r.Permissions = *data.Permissions
	}
	if data.Icon != nil {
		r.Icon = "hash-of-" + string(data.Icon.Data[len(data.Icon.Data)-8:])
	}
	g := guildID.String()
	f.roles[g] = append(f.roles[g], r)
	out := r.disgo()
	return &out, nil
}

// UpdateRolePositions applies the positions given and records the call.
func (f *fakeOps) UpdateRolePositions(guildID snowflake.ID, u []discord.RolePositionUpdate) ([]discord.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reorders = append(f.reorders, u)
	var out []discord.Role
	for _, moved := range u {
		for _, r := range f.roles[guildID.String()] {
			if core.ID(r.ID) == moved.ID && moved.Position != nil {
				r.Position = *moved.Position
				out = append(out, r.disgo())
			}
		}
	}
	return out, nil
}

func (f *fakeOps) AddMemberRole(guildID, userID, roleID snowflake.ID, _ ...rest.RequestOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, u, r := guildID.String(), userID.String(), roleID.String()
	f.roleAddCalls = append(f.roleAddCalls, g+":"+u+":"+r)
	if m, ok := f.members[memberKey(g, u)]; ok && !slices.Contains(m.Roles, r) {
		m.Roles = append(m.Roles, r)
	}
	return nil
}

func (f *fakeOps) RemoveMemberRole(guildID, userID, roleID snowflake.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, u, r := guildID.String(), userID.String(), roleID.String()
	f.roleRemoveCalls = append(f.roleRemoveCalls, g+":"+u+":"+r)
	if m, ok := f.members[memberKey(g, u)]; ok {
		m.Roles = slices.DeleteFunc(m.Roles, func(id string) bool { return id == r })
	}
	return nil
}

func (f *fakeOps) GetChannel(channelID snowflake.ID) (discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channel[channelID.String()]
	if !ok {
		return nil, fmt.Errorf("fakeOps: unknown channel %s", channelID)
	}
	return ch.Disgo(), nil
}

func (f *fakeOps) GetAuditLog(guildID, userID snowflake.ID, action discord.AuditLogEvent, before, after snowflake.ID, limit int) (*discord.AuditLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditLogErr != nil {
		return nil, f.auditLogErr
	}
	var out []discord.AuditLogEntry
	for _, e := range f.auditLog {
		if e.ActionType == action && e.UserID == userID {
			out = append(out, e)
		}
	}
	return &discord.AuditLog{AuditLogEntries: out}, nil
}

func (f *fakeOps) GetGuildChannels(guildID snowflake.ID) ([]discord.GuildChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.guildChannelsErr != nil {
		return nil, f.guildChannelsErr
	}
	var out []discord.GuildChannel
	for _, ch := range f.channel {
		if ch.GuildID == guildID.String() {
			out = append(out, ch.Disgo())
		}
	}
	return out, nil
}

// UpdatePermissionOverwrite records the write and mirrors it onto the
// channel's own overwrites, the way Discord does. The callers read those
// back to skip writes that would change nothing, so a fake that only kept a
// side map would report every repeat sync as still needed.
func (f *fakeOps) UpdatePermissionOverwrite(channelID, targetID snowflake.ID, u discord.PermissionOverwriteUpdate) error {
	var allow, deny discord.Permissions
	var kind discord.PermissionOverwriteType
	switch v := u.(type) {
	case discord.RolePermissionOverwriteUpdate:
		kind, allow, deny = discord.PermissionOverwriteTypeRole, permOf(v.Allow), permOf(v.Deny)
	case discord.MemberPermissionOverwriteUpdate:
		kind, allow, deny = discord.PermissionOverwriteTypeMember, permOf(v.Allow), permOf(v.Deny)
	}
	c, t := channelID.String(), targetID.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permSetCalls++
	f.overwrites[overwriteKey{c, t}] = permPair{allow, deny}
	ch, ok := f.channel[c]
	if !ok {
		return nil
	}
	if ow := discordtest.Find(ch.PermissionOverwrites, t, kind); ow != nil {
		ow.Allow, ow.Deny = allow, deny
		return nil
	}
	ch.PermissionOverwrites = append(ch.PermissionOverwrites, &fow{ID: t, Type: kind, Allow: allow, Deny: deny})
	return nil
}

func (f *fakeOps) DeletePermissionOverwrite(channelID, targetID snowflake.ID) error {
	c, t := channelID.String(), targetID.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permDeleteCalls++
	delete(f.overwrites, overwriteKey{c, t})
	if ch, ok := f.channel[c]; ok {
		ch.PermissionOverwrites = slices.DeleteFunc(ch.PermissionOverwrites, func(ow *fow) bool { return ow.ID == t })
	}
	return nil
}

func permOf(p *discord.Permissions) discord.Permissions {
	if p == nil {
		return 0
	}
	return *p
}

// --- fakeStore: in-memory Store ---

type fakeStore struct {
	mu sync.Mutex
	// insertJailErr, when set, fails every InsertJail, for testing what the
	// jail mutation leaves behind when the record can't be written.
	insertJailErr error
	// setJailReleaseErr, when set, fails every SetJailRelease, for testing
	// what a re-jail reports when the sentence can't be moved.
	setJailReleaseErr error
	// transferJailErr, when set, fails every TransferJail, for testing what
	// a transfer leaves behind when the row cannot be moved.
	transferJailErr error
	// The read-side errors, one per query, for the paths that must fail
	// closed (a timer or sweep that cannot read its row does nothing).
	getJailErr     error
	getGrantErr    error
	dueJailsErr    error
	dueGrantsErr   error
	activeJailsErr error
	jails          map[string]JailRecord        // guildID+":"+userID
	grants         map[string]GrantRecord       // guildID+":"+userID+":"+roleID
	eternal        map[string]EternalRoleRecord // guildID+":"+userID+":"+originRoleID
	nextID         int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{jails: make(map[string]JailRecord), grants: make(map[string]GrantRecord), eternal: make(map[string]EternalRoleRecord)}
}

func (f *fakeStore) ListEternalRoles(ctx context.Context, guildID string) ([]EternalRoleRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []EternalRoleRecord
	for _, rec := range f.eternal {
		if rec.GuildID == guildID {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b EternalRoleRecord) int { return a.CapturedAt.Compare(b.CapturedAt) })
	return out, nil
}

func (f *fakeStore) DeleteEternalRole(ctx context.Context, guildID, userID, originRoleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.eternal, guildID+":"+userID+":"+originRoleID)
	return nil
}

func (f *fakeStore) PutEternalRole(ctx context.Context, rec EternalRoleRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eternal[rec.GuildID+":"+rec.UserID+":"+rec.OriginRoleID] = rec
	return nil
}

// fakeScripts is an in-memory scripts.Store: on is the set of enabled
// script names per guild, err fails every read.
type fakeScripts struct {
	on  map[string]bool // guildID+":"+script
	err error
}

func newFakeScripts() *fakeScripts { return &fakeScripts{on: make(map[string]bool)} }

func (f *fakeScripts) Enabled(ctx context.Context, guildID, script string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.on[guildID+":"+script], nil
}

func (f *fakeScripts) SetEnabled(ctx context.Context, guildID, script string, on bool) error {
	f.on[guildID+":"+script] = on
	return nil
}

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

func (f *fakeStore) SetJailRelease(ctx context.Context, guildID, userID string, releaseAt *time.Time) error {
	if f.setJailReleaseErr != nil {
		return f.setJailReleaseErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := jailKey(guildID, userID)
	rec, ok := f.jails[key]
	if !ok {
		return fmt.Errorf("fake store: set jail release for %s: %w", userID, ErrNotJailed)
	}
	// Mirrors the real UPDATE: release_at only, never the snapshot.
	rec.ReleaseAt = releaseAt
	f.jails[key] = rec
	return nil
}

func (f *fakeStore) TransferJail(ctx context.Context, guildID, userID, jailRoleID string, releaseAt *time.Time) error {
	if f.transferJailErr != nil {
		return f.transferJailErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := jailKey(guildID, userID)
	rec, ok := f.jails[key]
	if !ok {
		return fmt.Errorf("fake store: transfer jail for %s: %w", userID, ErrNotJailed)
	}
	// Mirrors the real UPDATE: marker and release_at only, never the snapshot.
	rec.JailRoleID, rec.ReleaseAt = jailRoleID, releaseAt
	f.jails[key] = rec
	return nil
}

func (f *fakeStore) MarkJailApplied(ctx context.Context, guildID, userID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := jailKey(guildID, userID)
	rec, ok := f.jails[key]
	if !ok {
		return fmt.Errorf("fake store: mark jail applied for %s: %w", userID, ErrNotJailed)
	}
	rec.JailedAt = at
	f.jails[key] = rec
	return nil
}

func (f *fakeStore) GetJail(ctx context.Context, guildID, userID string) (JailRecord, bool, error) {
	if f.getJailErr != nil {
		return JailRecord{}, false, f.getJailErr
	}
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
	if f.dueJailsErr != nil {
		return nil, f.dueJailsErr
	}
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
	if f.activeJailsErr != nil {
		return nil, f.activeJailsErr
	}
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
	if f.getGrantErr != nil {
		return GrantRecord{}, false, f.getGrantErr
	}
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
	if f.dueGrantsErr != nil {
		return nil, f.dueGrantsErr
	}
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
	mu              sync.Mutex
	allowed         map[string][]string // guildID -> channel IDs
	markerRole      map[string]string   // guildID -> configured jail marker role ID
	announce        map[string]string   // guildID -> configured jail announcement channel ID
	vacationRole    map[string]string   // guildID -> configured vacation marker role ID
	vacationAllowed map[string][]string // guildID -> vacation allowlist
	memberRole      map[string]string   // guildID -> ordinary member role
	joinRoles       map[string][]string // guildID -> extra join roles
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{allowed: make(map[string][]string), markerRole: make(map[string]string), announce: make(map[string]string), vacationRole: make(map[string]string), vacationAllowed: make(map[string][]string), memberRole: make(map[string]string), joinRoles: make(map[string][]string)}
}

func (f *fakeSettings) JailAnnounceChannelID(guildID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.announce[guildID]
}

func (f *fakeSettings) SetJailAnnounceChannel(ctx context.Context, guildID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if channelID == "" {
		delete(f.announce, guildID)
		return nil
	}
	f.announce[guildID] = channelID
	return nil
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

func (f *fakeSettings) VacationAllowedChannelIDs(guildID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.vacationAllowed[guildID]...)
}

func (f *fakeSettings) AddVacationAllowedChannel(ctx context.Context, guildID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.vacationAllowed[guildID], channelID) {
		f.vacationAllowed[guildID] = append(f.vacationAllowed[guildID], channelID)
	}
	return nil
}

func (f *fakeSettings) RemoveVacationAllowedChannel(ctx context.Context, guildID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vacationAllowed[guildID] = slices.DeleteFunc(f.vacationAllowed[guildID], func(id string) bool { return id == channelID })
	return nil
}

func (f *fakeSettings) MemberRoleID(guildID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memberRole[guildID]
}

func (f *fakeSettings) SetMemberRole(ctx context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if roleID == "" {
		delete(f.memberRole, guildID)
		return nil
	}
	f.memberRole[guildID] = roleID
	return nil
}

func (f *fakeSettings) JoinRoleIDs(guildID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.joinRoles[guildID])
}

func (f *fakeSettings) AddJoinRole(ctx context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.joinRoles[guildID], roleID) {
		f.joinRoles[guildID] = append(f.joinRoles[guildID], roleID)
	}
	return nil
}

func (f *fakeSettings) RemoveJoinRole(ctx context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinRoles[guildID] = slices.DeleteFunc(f.joinRoles[guildID], func(id string) bool { return id == roleID })
	return nil
}

func (f *fakeSettings) VacationRoleID(guildID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vacationRole[guildID]
}

func (f *fakeSettings) SetVacationRole(ctx context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if roleID == "" {
		delete(f.vacationRole, guildID)
		return nil
	}
	f.vacationRole[guildID] = roleID
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
	// bootstrapID is the one identity nothing may target, not even with the
	// consent flag JailAutomatic otherwise honours.
	bootstrapID string
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

// bootstrapID stands in for MERLIN_BOOTSTRAP_ADMIN_USER_ID. Empty in most
// tests: only JailAutomatic consults it.
func (f *fakePerms) IsBootstrapAdmin(userID string) bool {
	return f.bootstrapID != "" && userID == f.bootstrapID
}

func (f *fakePerms) CanModerate(guildID string, actor *core.Actor, targetUserID string, targetRoleIDs []string) error {
	if f.moderateErr != nil {
		return f.moderateErr
	}
	if f.protected[targetUserID] {
		return core.ErrForbidden{Reason: "target is an admin, only another admin can do that"}
	}
	return nil
}
