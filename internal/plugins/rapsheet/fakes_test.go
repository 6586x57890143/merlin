package rapsheet

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/discordtest"
	"github.com/6586x57890143/merlin/internal/voice"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testNow is a fixed clock every test starts from, so decay arithmetic in
// assertions is exact.
var testNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// --- store ------------------------------------------------------------------

// fakeStore is the in-memory Store. It models the two things Postgres
// enforces that the code relies on: the partial unique index on (guild,
// source, ref), and the predicate behind the ban-due index.
type fakeStore struct {
	mu sync.Mutex
	// now is the clock ActiveBan judges expiry by, wired to the plugin's own
	// so a 1d ban cut at testNow does not expire on the wall clock a day after
	// the test was written, which is precisely how it took CI down once.
	now     func() time.Time
	nextID  int64
	configs map[string]Config
	entries []Entry
	cases   map[string]CaseFile // guild|user
	links   map[string]Link     // guild|user
	hints   map[string]AltHint  // guild|user|candidate

	// Armable failures.
	insertErr  error
	configErr  error
	entriesErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		configs: map[string]Config{},
		cases:   map[string]CaseFile{},
		now:     func() time.Time { return testNow },
		links:   map[string]Link{},
		hints:   map[string]AltHint{},
	}
}

func key(parts ...string) string { return strings.Join(parts, "|") }

func (f *fakeStore) Config(_ context.Context, guildID string) (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return Config{}, f.configErr
	}
	if cfg, ok := f.configs[guildID]; ok {
		return cfg, nil
	}
	return defaultConfig(guildID), nil
}

func (f *fakeStore) SetConfig(_ context.Context, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[cfg.GuildID] = cfg
	return nil
}

func (f *fakeStore) Insert(_ context.Context, e Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	e = normalizeEntry(e)
	if !validCategory(e.Category) {
		return 0, errors.New("fake store: category check")
	}
	if e.Ref != "" {
		for _, x := range f.entries {
			if x.GuildID == e.GuildID && x.Source == e.Source && x.Ref == e.Ref {
				return 0, nil
			}
		}
	}
	f.nextID++
	e.ID = f.nextID
	f.entries = append(f.entries, e)
	return e.ID, nil
}

func (f *fakeStore) Entry(_ context.Context, guildID string, id int64) (Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.entries {
		if e.GuildID == guildID && e.ID == id {
			return e, nil
		}
	}
	return Entry{}, ErrNoEntry
}

func (f *fakeStore) Entries(_ context.Context, guildID string, userIDs []string, since time.Time, limit int) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entriesErr != nil {
		return nil, f.entriesErr
	}
	var out []Entry
	for _, e := range f.entries {
		if e.GuildID == guildID && slices.Contains(userIDs, e.UserID) && e.CreatedAt.After(since) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) EntryByRef(_ context.Context, guildID string, source Source, ref string) (Entry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == "" {
		return Entry{}, false, nil
	}
	for _, e := range f.entries {
		if e.GuildID == guildID && e.Source == source && e.Ref == ref {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

func (f *fakeStore) update(guildID string, id int64, fn func(*Entry)) error {
	for i := range f.entries {
		if (guildID == "" || f.entries[i].GuildID == guildID) && f.entries[i].ID == id {
			fn(&f.entries[i])
			return nil
		}
	}
	return ErrNoEntry
}

func (f *fakeStore) UpdateReason(_ context.Context, guildID string, id int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.update(guildID, id, func(e *Entry) { e.Reason = reason })
}

func (f *fakeStore) Void(_ context.Context, guildID string, id int64, by, reason string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	err := f.update(guildID, id, func(e *Entry) {
		if e.VoidedAt != nil {
			return
		}
		found = true
		t := at
		e.VoidedAt, e.VoidedBy, e.VoidReason = &t, by, reason
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrNoEntry
	}
	return nil
}

func (f *fakeStore) SetThreadMessage(_ context.Context, id int64, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.update("", id, func(e *Entry) { e.ThreadMessageID = messageID })
}

func (f *fakeStore) CountUnmirrored(_ context.Context, guildID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		if e.GuildID == guildID && e.ThreadMessageID == "" {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) CountScored(_ context.Context, guildID string, userIDs []string, since time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		if e.GuildID == guildID && slices.Contains(userIDs, e.UserID) && e.CreatedAt.After(since) && e.Points > 0 && e.VoidedAt == nil {
			n++
		}
	}
	return n, nil
}

func banDuePred(e Entry) bool {
	return e.Kind == KindBan && e.EndsAt != nil && e.LiftedAt == nil && e.VoidedAt == nil
}

func (f *fakeStore) DueBans(_ context.Context, guildID string, now time.Time) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Entry
	for _, e := range f.entries {
		if e.GuildID == guildID && banDuePred(e) && !e.EndsAt.After(now) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) CountPendingBans(_ context.Context, guildID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		if e.GuildID == guildID && banDuePred(e) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) MarkLifted(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.update("", id, func(e *Entry) {
		if e.LiftedAt == nil {
			t := at
			e.LiftedAt = &t
		}
	})
}

func (f *fakeStore) ActiveBan(_ context.Context, guildID, userID string) (Entry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best Entry
	found := false
	for _, e := range f.entries {
		if e.GuildID == guildID && e.UserID == userID && e.Kind == KindBan && e.LiftedAt == nil && e.VoidedAt == nil &&
			(e.EndsAt == nil || e.EndsAt.After(f.now())) {
			if !found || e.CreatedAt.After(best.CreatedAt) {
				best, found = e, true
			}
		}
	}
	return best, found, nil
}

func (f *fakeStore) RecentActioned(_ context.Context, guildID string, since time.Time) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Entry
	for _, e := range f.entries {
		if e.GuildID == guildID && (e.Kind == KindJail || e.Kind == KindBan || e.Kind == KindKick) && e.VoidedAt == nil && e.CreatedAt.After(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) GuildEntries(_ context.Context, guildID string, since time.Time, limit int) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Entry
	for _, e := range f.entries {
		if e.GuildID == guildID && e.CreatedAt.After(since) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) CaseFile(_ context.Context, guildID, userID string) (CaseFile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cf, ok := f.cases[key(guildID, userID)]
	return cf, ok, nil
}

func (f *fakeStore) UpsertCaseFile(_ context.Context, cf CaseFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(cf.GuildID, cf.UserID)
	if old, ok := f.cases[k]; ok {
		if cf.Username == "" {
			cf.Username = old.Username
		}
		if cf.GlobalName == "" {
			cf.GlobalName = old.GlobalName
		}
		if cf.AvatarHash == "" {
			cf.AvatarHash = old.AvatarHash
		}
		cf.ThreadID, cf.CreatedAt = old.ThreadID, old.CreatedAt
	} else {
		cf.CreatedAt = time.Now()
	}
	cf.UpdatedAt = time.Now()
	f.cases[k] = cf
	return nil
}

func (f *fakeStore) SetCaseThread(_ context.Context, guildID, userID, threadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(guildID, userID)
	cf := f.cases[k]
	cf.ThreadID = threadID
	f.cases[k] = cf
	return nil
}

func (f *fakeStore) CaseFiles(_ context.Context, guildID string, limit int) ([]CaseFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []CaseFile
	for _, cf := range f.cases {
		if cf.GuildID == guildID {
			out = append(out, cf)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) Link(_ context.Context, l Link) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l.CreatedAt = time.Now()
	f.links[key(l.GuildID, l.UserID)] = l
	return nil
}

func (f *fakeStore) Unlink(_ context.Context, guildID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.links, key(guildID, userID))
	return nil
}

func (f *fakeStore) Group(_ context.Context, guildID, userID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{userID}
	me, ok := f.links[key(guildID, userID)]
	if !ok {
		return out, nil
	}
	for _, l := range f.links {
		if l.GuildID == guildID && l.GroupID == me.GroupID && l.UserID != userID {
			out = append(out, l.UserID)
		}
	}
	sort.Strings(out[1:])
	return out, nil
}

func (f *fakeStore) GroupLinks(_ context.Context, guildID, userID string) ([]Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	me, ok := f.links[key(guildID, userID)]
	if !ok {
		return nil, nil
	}
	var out []Link
	for _, l := range f.links {
		if l.GuildID == guildID && l.GroupID == me.GroupID {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (f *fakeStore) UpsertHint(_ context.Context, h AltHint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(h.GuildID, h.UserID, h.CandidateID)
	if old, ok := f.hints[k]; ok && h.Opinion == "" {
		h.Opinion = old.Opinion
	}
	h.CreatedAt = time.Now()
	f.hints[k] = h
	return nil
}

func (f *fakeStore) Hints(_ context.Context, guildID, userID string) ([]AltHint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AltHint
	for _, h := range f.hints {
		if h.GuildID == guildID && (h.UserID == userID || h.CandidateID == userID) {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

func (f *fakeStore) DeleteHint(_ context.Context, guildID, userID, candidateID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hints, key(guildID, userID, candidateID))
	delete(f.hints, key(guildID, candidateID, userID))
	return nil
}

// all is every entry, in insertion order, for assertions.
func (f *fakeStore) all() []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Entry(nil), f.entries...)
}

// --- discord ----------------------------------------------------------------

// fch is the shared channel fixture.
type fch = discordtest.Channel

// edit is one UpdateMessage call.
type edit struct {
	channelID, messageID string
	u                    discord.MessageUpdate
}

// fakeOps records every Discord call and can be armed to fail.
type fakeOps struct {
	mu      sync.Mutex
	members map[string]*discord.Member // userID -> member; absent = unknown member
	users   map[string]*discord.User
	sent    map[string][]discord.MessageCreate // channelID -> sends
	dms     map[string]bool                    // DM channels handed out
	dmOpen  int

	channels      map[string]*fch
	created       []discord.GuildForumChannelCreate
	edits         []edit
	threadsOpened int

	roles    []discord.Role
	timeouts map[string]*time.Time // userID -> until (nil = cleared)
	bans     map[string]string     // userID -> reason
	kicked   []string
	unbanned []string

	memberErr   error
	dmErr       error
	sendErr     error
	channelsErr error
	createErr   error
	threadErr   error
	editErr     error
	timeoutErr  error
	banErr      error
	unbanErr    error
	kickErr     error
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		members:  map[string]*discord.Member{},
		users:    map[string]*discord.User{},
		sent:     map[string][]discord.MessageCreate{},
		dms:      map[string]bool{},
		channels: map[string]*fch{},
		timeouts: map[string]*time.Time{},
		bans:     map[string]string{},
	}
}

func (f *fakeOps) TimeoutMember(_, userID snowflake.ID, until *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timeoutErr != nil {
		return f.timeoutErr
	}
	f.timeouts[userID.String()] = until
	return nil
}

func (f *fakeOps) AddBan(_, userID snowflake.ID, _ time.Duration, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.banErr != nil {
		return f.banErr
	}
	f.bans[userID.String()] = reason
	delete(f.members, userID.String())
	return nil
}

func (f *fakeOps) DeleteBan(_, userID snowflake.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unbanErr != nil {
		return f.unbanErr
	}
	id := userID.String()
	if _, ok := f.bans[id]; !ok {
		return unknownErr(int(rest.JSONErrorCodeUnknownBan))
	}
	delete(f.bans, id)
	f.unbanned = append(f.unbanned, id)
	return nil
}

func (f *fakeOps) RemoveMember(_, userID snowflake.ID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kickErr != nil {
		return f.kickErr
	}
	id := userID.String()
	if _, ok := f.members[id]; !ok {
		return unknownMemberErr()
	}
	delete(f.members, id)
	f.kicked = append(f.kicked, id)
	return nil
}

func (f *fakeOps) GetRoles(snowflake.ID) ([]discord.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roles, nil
}

// addForum registers an existing forum channel.
func (f *fakeOps) addForum(id string) *fch {
	ch := &fch{ID: id, Name: "forum-" + id, Type: discord.ChannelTypeGuildForum}
	f.mu.Lock()
	f.channels[id] = ch
	f.mu.Unlock()
	return ch
}

func unknownErr(code int) error {
	return &rest.Error{Code: rest.JSONErrorCode(code), Message: "Unknown", Response: &http.Response{StatusCode: http.StatusNotFound}}
}

func (f *fakeOps) addMember(id string, roles ...string) *discord.Member {
	m := &discord.Member{User: discord.User{ID: core.ID(id), Username: "user-" + id}, RoleIDs: core.IDs(roles)}
	f.mu.Lock()
	f.members[id] = m
	f.users[id] = &m.User
	f.mu.Unlock()
	return m
}

func unknownMemberErr() error { return unknownErr(int(rest.JSONErrorCodeUnknownMember)) }

func (f *fakeOps) GetGuild(guildID snowflake.ID) (*discord.RestGuild, error) {
	g := &discord.RestGuild{}
	g.ID, g.Name = guildID, "Test Guild"
	return g, nil
}

func (f *fakeOps) GetMember(_, userID snowflake.ID) (*discord.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	m, ok := f.members[userID.String()]
	if !ok {
		return nil, unknownMemberErr()
	}
	return m, nil
}

func (f *fakeOps) GetUser(userID snowflake.ID) (*discord.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[userID.String()]; ok {
		return u, nil
	}
	return nil, unknownErr(int(rest.JSONErrorCodeUnknownUser))
}

// dmWith is the DM channel the fake opens with userID: the user's ID with
// a bit set that no fixture uses.
func dmWith(userID string) string { return strconv.FormatUint(uint64(core.ID(userID))|1<<62, 10) }

func (f *fakeOps) CreateDMChannel(recipientID snowflake.ID) (*discord.DMChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmErr != nil {
		return nil, f.dmErr
	}
	f.dmOpen++
	id := dmWith(recipientID.String())
	f.dms[id] = true
	return discordtest.DM(id), nil
}

func (f *fakeOps) CreateMessage(channelID snowflake.ID, data discord.MessageCreate) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	id := channelID.String()
	if !f.dms[id] {
		if _, ok := f.channels[id]; !ok {
			return nil, unknownErr(int(rest.JSONErrorCodeUnknownChannel))
		}
	}
	f.sent[id] = append(f.sent[id], data)
	return &discord.Message{ID: snowflake.ID(8000 + len(f.sent[id])), ChannelID: channelID}, nil
}

func (f *fakeOps) GetChannel(channelID snowflake.ID) (discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.channels[channelID.String()]; ok {
		return ch.Disgo(), nil
	}
	return nil, unknownErr(int(rest.JSONErrorCodeUnknownChannel))
}

func (f *fakeOps) GetGuildChannels(snowflake.ID) ([]discord.GuildChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.channelsErr != nil {
		return nil, f.channelsErr
	}
	out := make([]discord.GuildChannel, 0, len(f.channels))
	for _, ch := range f.channels {
		out = append(out, ch.Disgo())
	}
	return out, nil
}

func (f *fakeOps) CreateGuildChannel(guildID snowflake.ID, c discord.GuildChannelCreate) (discord.GuildChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	forum, ok := c.(discord.GuildForumChannelCreate)
	if !ok {
		return nil, fmt.Errorf("fakeOps: rapsheet creates forums, got %T", c)
	}
	ch := &fch{ID: strconv.Itoa(7400000 + len(f.channels) + 1), GuildID: guildID.String(), Name: forum.Name, Type: forum.Type(),
		PermissionOverwrites: discordtest.Overwrites(forum.PermissionOverwrites)}
	f.channels[ch.ID] = ch
	f.created = append(f.created, forum)
	return ch.Disgo(), nil
}

func (f *fakeOps) CreatePostInThreadChannel(channelID snowflake.ID, c discord.ThreadChannelPostCreate) (*discord.ThreadChannelPost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.threadErr != nil {
		return nil, f.threadErr
	}
	if _, ok := f.channels[channelID.String()]; !ok {
		return nil, unknownErr(int(rest.JSONErrorCodeUnknownChannel))
	}
	f.threadsOpened++
	id := threadID(f.threadsOpened)
	th := &fch{ID: id, Name: c.Name, ParentID: channelID.String(), Type: discord.ChannelTypeGuildPublicThread}
	f.channels[id] = th
	f.sent[id] = append(f.sent[id], c.Message)
	return &discord.ThreadChannelPost{GuildThread: th.Thread()}, nil
}

// threadID is the nth thread the fake opens.
func threadID(n int) string { return strconv.Itoa(7500000 + n) }

func (f *fakeOps) UpdateMessage(channelID, messageID snowflake.ID, u discord.MessageUpdate) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.edits = append(f.edits, edit{channelID: channelID.String(), messageID: messageID.String(), u: u})
	return &discord.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeOps) sentTo(channelID string) []discord.MessageCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]discord.MessageCreate(nil), f.sent[channelID]...)
}

func strp(s string) *string { return &s }

func ptrTime(t time.Time) *time.Time { return &t }

// fakeSched records registrations so the "a job exists only where it has
// work" rule can be asserted directly.
type fakeSched struct {
	mu   sync.Mutex
	jobs map[string]func(context.Context) error
}

func newFakeSched() *fakeSched { return &fakeSched{jobs: map[string]func(context.Context) error{}} }

func (f *fakeSched) Register(key string, _ core.CronSpec, fn func(context.Context) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.jobs[key]; dup {
		return errors.New("duplicate job key")
	}
	f.jobs[key] = fn
	return nil
}

func (f *fakeSched) Unregister(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, key)
	return nil
}

func (f *fakeSched) RunNow(ctx context.Context, key string) error {
	f.mu.Lock()
	fn := f.jobs[key]
	f.mu.Unlock()
	if fn == nil {
		return errors.New("no such job")
	}
	return fn(ctx)
}

func (f *fakeSched) Seed(context.Context, string, time.Time) error { return nil }

func (f *fakeSched) NextDue(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeSched) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.jobs[key]
	return ok
}

// --- permissions --------------------------------------------------------------

// fakeRanker stands in for *core.Permissions. admins are admin-equivalent
// targets; bootstrap is the operator.
type fakeRanker struct {
	admins    map[string]bool
	bootstrap string
	authErr   error
	moderr    error
}

func newFakeRanker() *fakeRanker { return &fakeRanker{admins: map[string]bool{}} }

func (f *fakeRanker) CanModerate(_ string, actor *core.Actor, targetUserID string, _ []string) error {
	if f.moderr != nil {
		return f.moderr
	}
	if targetUserID == f.bootstrap {
		return core.ErrForbidden{Reason: "target is the bootstrap admin"}
	}
	if !f.admins[targetUserID] {
		return nil
	}
	if actor == nil || !f.admins[actor.ID] {
		return core.ErrForbidden{Reason: "target is an admin"}
	}
	return nil
}

func (f *fakeRanker) IsBootstrapAdmin(userID string) bool { return userID == f.bootstrap }

func (f *fakeRanker) AuthorizeIx(*core.Ix, core.PermSpec) error { return f.authErr }

// --- audit / voice / gate ----------------------------------------------------

type fakeAudit struct {
	mu   sync.Mutex
	rows []string
}

func (f *fakeAudit) Record(_ context.Context, guildID, actorID, action, old, newVal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, guildID+"|"+actorID+"|"+action+"|"+old+"|"+newVal)
	return nil
}

func (f *fakeAudit) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rows...)
}

func (f *fakeAudit) has(action string) bool {
	for _, r := range f.all() {
		if strings.Contains(r, "|"+action+"|") {
			return true
		}
	}
	return false
}

// fixedVoice always says the same thing, which is what makes DM assertions
// exact. It still records the key asked for.
type fixedVoice struct {
	mu   sync.Mutex
	line string
	keys []voice.Key
}

func (v *fixedVoice) Line(_ context.Context, _ string, k voice.Key, _ map[string]string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys = append(v.keys, k)
	return v.line
}

type fakeGate struct{ disabled map[string]bool }

func (g fakeGate) PluginEnabled(guildID, _ string) bool { return !g.disabled[guildID] }

type fakeModRoles []string

func (m fakeModRoles) ModRoleIDs(string) []string { return m }

// --- plugin -------------------------------------------------------------------

type harness struct {
	p      *Plugin
	store  *fakeStore
	ops    *fakeOps
	ranker *fakeRanker
	audit  *fakeAudit
	voice  *fixedVoice
	bus    *core.EventBus
	sched  *fakeSched
}

func newHarness() *harness {
	h := &harness{
		store:  newFakeStore(),
		ops:    newFakeOps(),
		ranker: newFakeRanker(),
		audit:  &fakeAudit{},
		voice:  &fixedVoice{line: "a line"},
		bus:    core.NewEventBus(quietLog()),
		sched:  newFakeSched(),
	}
	h.p = New(h.store, func(string) DiscordOps { return h.ops }, fakeModRoles{"7300018"}, h.voice)
	h.p.audit = h.audit
	h.p.log = quietLog()
	h.p.perms = h.ranker
	h.p.bus = h.bus
	h.p.sched = h.sched
	h.p.botID = core.ID("7300003")
	h.p.now = func() time.Time { return testNow }
	h.store.now = func() time.Time { return h.p.now() }
	h.p.synchronous = true
	h.p.subscribe()
	return h
}

// --- interactions -------------------------------------------------------------

type recordedCall struct {
	method, path, body string
}

// recordingTransport captures what a handler put on Discord's wire, so a
// test can assert on the sentence a moderator reads.
type recordingTransport struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	rt.mu.Lock()
	rt.calls = append(rt.calls, recordedCall{r.Method, r.URL.Path, textPart(body)})
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     http.StatusText(http.StatusOK),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"1"}`)),
		Request:    r,
	}, nil
}

func (rt *recordingTransport) said() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var b strings.Builder
	for _, c := range rt.calls {
		b.WriteString(c.body)
		b.WriteString("\n")
	}
	return b.String()
}

// textPart strips a multipart body down to its payload_json part, so a
// failing assertion prints the sentence and not the mood icon's PNG bytes.
func textPart(body string) string {
	if !strings.HasPrefix(body, "--") {
		return body
	}
	idx := strings.Index(body, `name="payload_json"`)
	if idx < 0 {
		return "<multipart without payload_json>"
	}
	rest := body[idx:]
	if i := strings.Index(rest, "\r\n\r\n"); i >= 0 {
		rest = rest[i+4:]
	}
	if i := strings.Index(rest, "\r\n--"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// current is the recorder the next interaction answers through. stubSession
// makes a fresh one per case, as it used to make a fresh session.
var current *recordingTransport

func stubSession() *recordingTransport {
	current = &recordingTransport{}
	return current
}

const (
	testGuild = "7300001"
	modID     = "7300004"
)

// payloads remembers what each test interaction was decoded from, so
// withResolved can add a resolved user the way Discord sends one.
var payloads = map[*core.Ix]built{}

type built struct {
	p    map[string]any
	user string
}

// decodeIx is Discord's JSON decoded by core.NewIx and answered through
// current, from a member shown as "mod".
//
// The fixtures predate disgo: the store and fakes key on "7300001", "7300002" and
// members like "7300004", none of them snowflakes. Ix.GuildID, UserID and
// ChannelID are merlin's own string keys and every handler reads those, so
// after decoding a real interaction they are pointed at the fixtures.
func decodeIx(p map[string]any, user string) *core.Ix {
	raw, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	rt := current
	if rt == nil {
		rt = &recordingTransport{}
	}
	ix, err := core.NewIx(raw, rest.New(rest.NewClient("test", rest.WithHTTPClient(&http.Client{Transport: rt}))))
	if err != nil {
		panic(err)
	}
	ix.GuildID, ix.UserID, ix.ChannelID = testGuild, user, "7300002"
	payloads[ix] = built{p, user}
	return ix
}

func payload(typ int, data map[string]any) map[string]any {
	return map[string]any{
		"id": "1000", "application_id": "2", "type": typ, "token": "tok", "version": 1,
		"guild_id": paneltest.IxGuild,
		"channel":  map[string]any{"id": "101", "type": 0},
		"member": map[string]any{
			"nick":        "mod",
			"user":        map[string]any{"id": "11", "username": "mod", "global_name": "mod"},
			"permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z",
		},
		"data": data,
	}
}

// interaction builds a command invocation for one leaf of /rapsheet, run
// by modID.
func interaction(leaf string, opts ...map[string]any) *core.Ix {
	return interactionBy(modID, leaf, opts...)
}

func interactionBy(userID, leaf string, opts ...map[string]any) *core.Ix {
	args := []any{}
	for _, o := range opts {
		args = append(args, o)
	}
	options := []any{map[string]any{"name": leaf, "type": 1, "options": args}}
	if group, sub, ok := strings.Cut(leaf, "/"); ok {
		options = []any{map[string]any{"name": group, "type": 2, "options": []any{
			map[string]any{"name": sub, "type": 1, "options": args},
		}}}
	}
	return decodeIx(payload(2, map[string]any{"id": "6", "name": "rapsheet", "type": 1, "options": options}), userID)
}

// withResolved attaches a resolved user, the way Discord does for a User
// option. Resolved users are keyed by snowflake, so u's ID must be one.
func withResolved(ix *core.Ix, u *discord.User) *core.Ix {
	b := payloads[ix]
	data := b.p["data"].(map[string]any)
	resolved, _ := data["resolved"].(map[string]any)
	if resolved == nil {
		resolved = map[string]any{"users": map[string]any{}}
		data["resolved"] = resolved
	}
	resolved["users"].(map[string]any)[u.ID.String()] = map[string]any{"id": u.ID.String(), "username": u.Username, "global_name": u.GlobalName, "avatar": u.Avatar, "bot": u.Bot}
	return decodeIx(b.p, b.user)
}

func strOpt(name, v string) map[string]any {
	return map[string]any{"name": name, "type": 3, "value": v}
}

func userOpt(name, id string) map[string]any {
	return map[string]any{"name": name, "type": 6, "value": id}
}

func intOpt(name string, v int) map[string]any {
	return map[string]any{"name": name, "type": 4, "value": v}
}

func boolOpt(name string, v bool) map[string]any {
	return map[string]any{"name": name, "type": 5, "value": v}
}

func componentClick(userID, customID string) *core.Ix {
	return decodeIx(payload(3, map[string]any{"custom_id": customID, "component_type": 2}), userID)
}

// panelClick is modID clicking customID on /rapsheet settings, picking
// values.
func panelClick(p *Plugin, customID string, values ...string) {
	data := map[string]any{"custom_id": customID, "component_type": 2}
	if values != nil {
		vs := []any{}
		for _, v := range values {
			vs = append(vs, v)
		}
		data["component_type"], data["values"] = 3, vs
	}
	p.handlePanelSet(context.Background(), decodeIx(payload(3, data), modID), customID)
}

// panelForm is modID submitting one of /rapsheet settings' forms.
func panelForm(p *Plugin, customID string, fields map[string]string) {
	rows := []any{}
	for k, v := range fields {
		rows = append(rows, map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": k, "value": v}}})
	}
	p.handleForm(context.Background(), decodeIx(payload(5, map[string]any{"custom_id": customID, "components": rows}), modID), customID)
}

// asMember is ix run by u, as Discord sends the invoking member's user.
func asMember(ix *core.Ix, u *discord.User) *core.Ix {
	b := payloads[ix]
	b.p["member"].(map[string]any)["user"] = map[string]any{"id": "11", "username": u.Username, "global_name": u.GlobalName, "avatar": u.Avatar}
	return decodeIx(b.p, u.ID.String())
}
