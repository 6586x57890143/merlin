package rapsheet

import (
	"context"
	"errors"
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
	mu      sync.Mutex
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
			(e.EndsAt == nil || e.EndsAt.After(time.Now())) {
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

// fakeOps records every Discord call and can be armed to fail.
type fakeOps struct {
	mu      sync.Mutex
	members map[string]*discordgo.Member // userID -> member; absent = unknown member
	users   map[string]*discordgo.User
	sent    map[string][]*discordgo.MessageSend // channelID -> sends
	dmOpen  int

	channels      map[string]*discordgo.Channel
	created       []discordgo.GuildChannelCreateData
	edits         []*discordgo.MessageEdit
	threadsOpened int

	roles    []*discordgo.Role
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
		members:  map[string]*discordgo.Member{},
		users:    map[string]*discordgo.User{},
		sent:     map[string][]*discordgo.MessageSend{},
		channels: map[string]*discordgo.Channel{},
		timeouts: map[string]*time.Time{},
		bans:     map[string]string{},
	}
}

func (f *fakeOps) GuildMemberTimeout(_, userID string, until *time.Time, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timeoutErr != nil {
		return f.timeoutErr
	}
	f.timeouts[userID] = until
	return nil
}

func (f *fakeOps) GuildBanCreateWithReason(_, userID, reason string, _ int, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.banErr != nil {
		return f.banErr
	}
	f.bans[userID] = reason
	delete(f.members, userID)
	return nil
}

func (f *fakeOps) GuildBanDelete(_, userID string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unbanErr != nil {
		return f.unbanErr
	}
	if _, ok := f.bans[userID]; !ok {
		return unknownErr(discordgo.ErrCodeUnknownBan)
	}
	delete(f.bans, userID)
	f.unbanned = append(f.unbanned, userID)
	return nil
}

func (f *fakeOps) GuildMemberDeleteWithReason(_, userID, _ string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kickErr != nil {
		return f.kickErr
	}
	if _, ok := f.members[userID]; !ok {
		return unknownMemberErr()
	}
	delete(f.members, userID)
	f.kicked = append(f.kicked, userID)
	return nil
}

func (f *fakeOps) GuildRoles(_ string, _ ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roles, nil
}

// addForum registers an existing forum channel.
func (f *fakeOps) addForum(id string) *discordgo.Channel {
	ch := &discordgo.Channel{ID: id, Name: "forum-" + id, Type: discordgo.ChannelTypeGuildForum}
	f.mu.Lock()
	f.channels[id] = ch
	f.mu.Unlock()
	return ch
}

func unknownErr(code int) error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Message:  &discordgo.APIErrorMessage{Code: code, Message: "Unknown"},
	}
}

func (f *fakeOps) addMember(id string, roles ...string) *discordgo.Member {
	m := &discordgo.Member{User: &discordgo.User{ID: id, Username: "user-" + id}, Roles: roles}
	f.mu.Lock()
	f.members[id] = m
	f.users[id] = m.User
	f.mu.Unlock()
	return m
}

func unknownMemberErr() error { return unknownErr(discordgo.ErrCodeUnknownMember) }

func (f *fakeOps) Guild(guildID string, _ ...discordgo.RequestOption) (*discordgo.Guild, error) {
	return &discordgo.Guild{ID: guildID, Name: "Test Guild"}, nil
}

func (f *fakeOps) GuildMember(_, userID string, _ ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	m, ok := f.members[userID]
	if !ok {
		return nil, unknownMemberErr()
	}
	return m, nil
}

func (f *fakeOps) User(userID string, _ ...discordgo.RequestOption) (*discordgo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[userID]; ok {
		return u, nil
	}
	return nil, errors.New("unknown user")
}

func (f *fakeOps) UserChannelCreate(recipientID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmErr != nil {
		return nil, f.dmErr
	}
	f.dmOpen++
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}

func (f *fakeOps) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	if !strings.HasPrefix(channelID, "dm-") {
		if _, ok := f.channels[channelID]; !ok {
			return nil, unknownErr(discordgo.ErrCodeUnknownChannel)
		}
	}
	f.sent[channelID] = append(f.sent[channelID], data)
	return &discordgo.Message{ID: fmt.Sprintf("m-%d", len(f.sent[channelID])), ChannelID: channelID}, nil
}

func (f *fakeOps) Channel(channelID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.channels[channelID]; ok {
		return ch, nil
	}
	return nil, unknownErr(discordgo.ErrCodeUnknownChannel)
}

func (f *fakeOps) GuildChannels(_ string, _ ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.channelsErr != nil {
		return nil, f.channelsErr
	}
	out := make([]*discordgo.Channel, 0, len(f.channels))
	for _, ch := range f.channels {
		out = append(out, ch)
	}
	return out, nil
}

func (f *fakeOps) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	ch := &discordgo.Channel{ID: fmt.Sprintf("ch-%d", len(f.channels)+1), GuildID: guildID, Name: data.Name, Type: data.Type, PermissionOverwrites: data.PermissionOverwrites}
	f.channels[ch.ID] = ch
	f.created = append(f.created, data)
	return ch, nil
}

func (f *fakeOps) ForumThreadStartComplex(channelID string, th *discordgo.ThreadStart, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.threadErr != nil {
		return nil, f.threadErr
	}
	if _, ok := f.channels[channelID]; !ok {
		return nil, unknownErr(discordgo.ErrCodeUnknownChannel)
	}
	f.threadsOpened++
	id := fmt.Sprintf("t-%d", f.threadsOpened)
	f.channels[id] = &discordgo.Channel{ID: id, Name: th.Name, ParentID: channelID, Type: discordgo.ChannelTypeGuildPublicThread}
	f.sent[id] = append(f.sent[id], data)
	return f.channels[id], nil
}

func (f *fakeOps) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.edits = append(f.edits, m)
	return &discordgo.Message{ID: m.ID, ChannelID: m.Channel}, nil
}

func (f *fakeOps) sentTo(channelID string) []*discordgo.MessageSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*discordgo.MessageSend(nil), f.sent[channelID]...)
}

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

func (f *fakeRanker) CanModerate(_ string, actor *discordgo.Member, targetUserID string, _ []string) error {
	if f.moderr != nil {
		return f.moderr
	}
	if targetUserID == f.bootstrap {
		return core.ErrForbidden{Reason: "target is the bootstrap admin"}
	}
	if !f.admins[targetUserID] {
		return nil
	}
	if actor == nil || actor.User == nil || !f.admins[actor.User.ID] {
		return core.ErrForbidden{Reason: "target is an admin"}
	}
	return nil
}

func (f *fakeRanker) IsBootstrapAdmin(userID string) bool { return userID == f.bootstrap }

func (f *fakeRanker) Authorize(*discordgo.InteractionCreate, core.PermSpec) error { return f.authErr }

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
	h.p = New(h.store, func(string) DiscordOps { return h.ops }, fakeModRoles{"mod-role"}, h.voice)
	h.p.audit = h.audit
	h.p.log = quietLog()
	h.p.perms = h.ranker
	h.p.bus = h.bus
	h.p.sched = h.sched
	h.p.botID = "merlin-1"
	h.p.now = func() time.Time { return testNow }
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

func stubSession() (*discordgo.Session, *recordingTransport) {
	rt := &recordingTransport{}
	return &discordgo.Session{
		Client:      &http.Client{Transport: rt},
		Ratelimiter: discordgo.NewRatelimiter(),
		Token:       "Bot test",
	}, rt
}

const (
	testGuild = "g1"
	modID     = "mod-1"
)

// interaction builds a command invocation for one leaf of /rapsheet, run
// by modID.
func interaction(leaf string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	return interactionBy(modID, leaf, opts...)
}

func interactionBy(userID, leaf string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	var options []*discordgo.ApplicationCommandInteractionDataOption
	if group, sub, ok := strings.Cut(leaf, "/"); ok {
		options = []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: group, Type: discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{
				Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
			}},
		}}
	} else {
		options = []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: leaf, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
		}}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:        "i1",
		Token:     "tok",
		GuildID:   testGuild,
		ChannelID: "chan-1",
		Type:      discordgo.InteractionApplicationCommand,
		Member: &discordgo.Member{
			Nick: "mod",
			User: &discordgo.User{ID: userID, Username: "mod", GlobalName: "mod"},
		},
		Data: discordgo.ApplicationCommandInteractionData{
			Name:     "rapsheet",
			Options:  options,
			Resolved: &discordgo.ApplicationCommandInteractionDataResolved{Users: map[string]*discordgo.User{}},
		},
	}}
}

// withResolved attaches a resolved user, the way Discord does for a User
// option.
func withResolved(i *discordgo.InteractionCreate, u *discordgo.User) *discordgo.InteractionCreate {
	data := i.ApplicationCommandData()
	data.Resolved.Users[u.ID] = u
	i.Data = data
	return i
}

func strOpt(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v}
}

func userOpt(name, id string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionUser, Value: id}
}

func intOpt(name string, v int) *discordgo.ApplicationCommandInteractionDataOption {
	// Discord sends numbers as float64 over JSON, and IntValue asserts
	// exactly that.
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(v)}
}

func boolOpt(name string, v bool) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionBoolean, Value: v}
}

func componentClick(userID, customID string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i3", Token: "tok", GuildID: testGuild, ChannelID: "chan-1",
		Type: discordgo.InteractionMessageComponent,
		Member: &discordgo.Member{
			Nick: "mod", User: &discordgo.User{ID: userID, Username: "mod"},
		},
		Data: discordgo.MessageComponentInteractionData{CustomID: customID},
	}}
}
