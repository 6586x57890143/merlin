package contest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordtest"
	"github.com/6586x57890143/merlin/internal/secret"
	"github.com/6586x57890143/merlin/internal/voice"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// botUserID is merlin's own snowflake in these tests. Every forum overwrite
// list carries an entry for her, so it needs a name rather than a literal
// repeated at a dozen assertions.
const botUserID = "1001"

// forumID is the forum the fake creates.
const forumID = "2001"

// guildRoles is a guild's role list as resolveAccess sees it: @everyone,
// whose ID is the guild's, plus whatever the test names.
func guildRoles(guildID string, ids ...string) []discord.Role {
	out := []discord.Role{{ID: core.ID(guildID), Name: "@everyone"}}
	for _, id := range ids {
		out = append(out, discord.Role{ID: core.ID(id), Name: id})
	}
	return out
}

// fakeStore is the whole Store interface backed by maps. Every plugin in
// this repo has one of these; the point is that the phase machine and the
// forum sync can be driven without Postgres.
type fakeStore struct {
	mu       sync.Mutex
	cfg      map[string]Config
	contests []Contest
	subs     map[string][]Submission
	prizes   map[string][]Prize

	// failures a test can arm, so the fail-closed paths are reachable.
	liveErr      error
	subsErr      error
	setForumFail error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cfg:    map[string]Config{},
		subs:   map[string][]Submission{},
		prizes: map[string][]Prize{},
	}
}

func (f *fakeStore) GetConfig(_ context.Context, guildID string) (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cfg[guildID]; ok {
		return c, nil
	}
	return Config{GuildID: guildID, DefaultMaxVotes: 3}, nil
}

func (f *fakeStore) SetConfig(_ context.Context, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg[cfg.GuildID] = cfg
	return nil
}

func (f *fakeStore) CreateContest(_ context.Context, c Contest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contests = append(f.contests, c)
	return nil
}

func (f *fakeStore) LiveContest(_ context.Context, guildID string) (Contest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveErr != nil {
		return Contest{}, f.liveErr
	}
	for i := len(f.contests) - 1; i >= 0; i-- {
		if f.contests[i].GuildID == guildID && f.contests[i].Live() {
			return f.contests[i], nil
		}
	}
	return Contest{}, ErrNoLiveContest
}

func (f *fakeStore) LatestContest(_ context.Context, guildID string) (Contest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.contests) - 1; i >= 0; i-- {
		if f.contests[i].GuildID == guildID && f.contests[i].Phase != PhaseCancelled {
			return f.contests[i], nil
		}
	}
	return Contest{}, ErrNoLiveContest
}

func (f *fakeStore) AdvancePhase(_ context.Context, contestID string, from, to Phase) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.contests {
		if f.contests[i].ID == contestID && f.contests[i].Phase == from {
			f.contests[i].Phase = to
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) find(contestID string) *Contest {
	for i := range f.contests {
		if f.contests[i].ID == contestID {
			return &f.contests[i]
		}
	}
	return nil
}

func (f *fakeStore) SetForumChannel(_ context.Context, contestID, channelID string) error {
	if f.setForumFail != nil {
		return f.setForumFail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(contestID); c != nil {
		c.ForumChannelID = channelID
	}
	return nil
}

func (f *fakeStore) SetAnnounceMessage(_ context.Context, contestID, channelID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(contestID); c != nil {
		c.AnnounceChannelID, c.AnnounceMessageID = channelID, messageID
	}
	return nil
}

func (f *fakeStore) SetResults(_ context.Context, contestID string, results []byte, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(contestID); c != nil {
		c.Results, c.ClosedAt, c.TallyError = results, &at, ""
	}
	return nil
}

func (f *fakeStore) SetTallyError(_ context.Context, contestID, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(contestID); c != nil {
		c.TallyError = msg
	}
	return nil
}

func (f *fakeStore) UpsertSubmission(_ context.Context, s Submission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.subs[s.ContestID]
	for i := range list {
		if list[i].ThreadID == s.ThreadID {
			s.ID, s.CreatedAt = list[i].ID, list[i].CreatedAt
			s.WithdrawnAt = nil // the real upsert resets withdrawn_at
			list[i] = s
			f.subs[s.ContestID] = list
			return nil
		}
	}
	// contest_submissions_one_live_idx: unique on (contest_id, user_id)
	// where withdrawn_at is null. Modelled here because the statement's own
	// ON CONFLICT covers thread_id only, so this collision surfaces as an
	// error rather than an update, and a fake that only knew about thread_id
	// made the upsert-before-withdraw ordering bug invisible to the suite.
	for i := range list {
		if list[i].UserID == s.UserID && list[i].WithdrawnAt == nil {
			return fmt.Errorf("fake store: contest_submissions_one_live_idx: %s already has a live entry", s.UserID)
		}
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Unix(int64(len(list)), 0)
	}
	f.subs[s.ContestID] = append(list, s)
	return nil
}

func (f *fakeStore) Submissions(_ context.Context, contestID string) ([]Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subsErr != nil {
		return nil, f.subsErr
	}
	var out []Submission
	for _, s := range f.subs[contestID] {
		if s.WithdrawnAt == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) WithdrawMissing(_ context.Context, contestID string, live []string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := make(map[string]bool, len(live))
	for _, id := range live {
		keep[id] = true
	}
	list := f.subs[contestID]
	for i := range list {
		if !keep[list[i].ThreadID] && list[i].WithdrawnAt == nil {
			t := at
			list[i].WithdrawnAt = &t
		}
	}
	return nil
}

func (f *fakeStore) AddPrize(_ context.Context, p Prize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prizes[p.ContestID] = append(f.prizes[p.ContestID], p)
	return nil
}

func (f *fakeStore) Prizes(_ context.Context, contestID string) ([]Prize, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Prize(nil), f.prizes[contestID]...), nil
}

func (f *fakeStore) PrizesAwardedTo(_ context.Context, guildID, userID string) ([]Prize, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	guilds := map[string]string{} // contest ID -> guild
	for _, c := range f.contests {
		guilds[c.ID] = c.GuildID
	}
	var out []Prize
	for cid, list := range f.prizes {
		if guilds[cid] != guildID {
			continue
		}
		for _, p := range list {
			if p.AwardedTo != nil && *p.AwardedTo == userID {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) RemovePrize(_ context.Context, contestID, prizeID, donorID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.prizes[contestID]
	for i, p := range list {
		if p.ID == prizeID && p.DonorID == donorID && p.AwardedAt == nil {
			f.prizes[contestID] = append(list[:i:i], list[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// ApprovePrize and RejectPrize model the store's conditional update, not just
// its effect: both refuse a pledge that has already been ruled on, because
// the review buttons rely on losing that claim rather than on the message
// they arrived from being fresh.
func (f *fakeStore) ApprovePrize(_ context.Context, prizeID, byUserID string, at time.Time) (bool, error) {
	return f.review(prizeID, byUserID, at, true), nil
}

func (f *fakeStore) RejectPrize(_ context.Context, prizeID, byUserID string, at time.Time) (bool, error) {
	return f.review(prizeID, byUserID, at, false), nil
}

func (f *fakeStore) review(prizeID, byUserID string, at time.Time, approved bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for cid := range f.prizes {
		for i := range f.prizes[cid] {
			pr := &f.prizes[cid][i]
			if pr.ID != prizeID || pr.ReviewedAt != nil {
				continue
			}
			t := at
			pr.ReviewedAt, pr.ReviewedBy, pr.Approved = &t, byUserID, approved
			if !approved {
				// Wiped in the same write as the decision, as the real
				// statement does: a fake that left it would let a test pass
				// on an ordering the database does not allow.
				pr.SecretSealed = nil
			}
			return true
		}
	}
	return false
}

func (f *fakeStore) MarkPrizeAwarded(_ context.Context, prizeID, winnerID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for cid := range f.prizes {
		for i := range f.prizes[cid] {
			if f.prizes[cid][i].ID == prizeID {
				w, t := winnerID, at
				f.prizes[cid][i].AwardedTo, f.prizes[cid][i].AwardedAt = &w, &t
				return nil
			}
		}
	}
	return nil
}

func (f *fakeStore) ClearPrizeSecret(_ context.Context, prizeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for cid := range f.prizes {
		for i := range f.prizes[cid] {
			if f.prizes[cid][i].ID == prizeID {
				f.prizes[cid][i].SecretSealed = nil
				return nil
			}
		}
	}
	return nil
}

// fakeOps records what merlin asked Discord to do, and lets a test arm a
// failure on any of it.
type fakeOps struct {
	mu sync.Mutex

	channels []*fch
	threads  []*fch
	messages map[string][]discord.Message // channel/thread ID -> messages

	created  []discord.GuildForumChannelCreate
	sent     map[string][]discord.MessageCreate
	overwrit []discord.Permissions // deny masks of every overwrite write, in order
	pinned   []string
	dms      map[string]bool // DM channels handed out

	// threadsAskedFor records the id GetActiveGuildThreads was called with,
	// so a test can tell the guild-scoped endpoint from the channel-scoped
	// one it replaced. The fake ignores it otherwise.
	threadsAskedFor string

	// archived is the other half of the forum: threads Discord returns only
	// from the channel-scoped archived endpoint, which is where a quiet
	// forum post ends up on its own.
	archived         []*fch
	archivedAskedFor string
	archivedPages    int

	// messageReads counts starter-message fetches, which is the expensive
	// half of a sync: one REST call per entry, per run.
	messageReads int

	roles []discord.Role
	// edits counts whole-list permission writes, which is how a test tells
	// "the resync changed something" from "it decided nothing had to change".
	edits int

	dmFails     bool
	rolesErr    error
	channelErr  error
	createFail  error
	threadsErr  error
	archivedErr error
}

// fch and fow are the shared fixtures, under short names.
type (
	fch = discordtest.Channel
	fow = discordtest.Overwrite
)

func newFakeOps() *fakeOps {
	return &fakeOps{
		messages: map[string][]discord.Message{},
		sent:     map[string][]discord.MessageCreate{},
		dms:      map[string]bool{},
	}
}

// channel is the stored channel by ID, nil if there is none.
func (f *fakeOps) channel(id string) *fch {
	for _, ch := range f.channels {
		if ch.ID == id {
			return ch
		}
	}
	return nil
}

// GetChannel hands back the stored channel, overwrites and all, rather than
// a bare shell with the right ID. setForumOpen reads its own channel back to
// change one bit inside the @everyone entry, so a fake that forgets what it
// was created with reports "nothing to change" for every call and the test
// passes while the real thing does the opposite.
func (f *fakeOps) GetChannel(id snowflake.ID) (discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.channelErr != nil {
		return nil, f.channelErr
	}
	if ch := f.channel(id.String()); ch != nil {
		return ch.Disgo(), nil
	}
	return (&fch{ID: id.String()}).Disgo(), nil
}

func (f *fakeOps) GetGuildChannels(snowflake.ID) ([]discord.GuildChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]discord.GuildChannel, len(f.channels))
	for i, ch := range f.channels {
		out[i] = ch.Disgo()
	}
	return out, nil
}

// GetRoles is what resolveAccess checks named roles against, so a test can
// make a role disappear the way a guild can.
func (f *fakeOps) GetRoles(snowflake.ID) ([]discord.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rolesErr != nil {
		return nil, f.rolesErr
	}
	return f.roles, nil
}

func (f *fakeOps) UpdateChannel(channelID snowflake.ID, u discord.ChannelUpdate) (discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits++
	data, ok := u.(discord.GuildTextChannelUpdate)
	if !ok {
		return nil, fmt.Errorf("fakeOps: contest sends GuildTextChannelUpdate, got %T", u)
	}
	ch := f.channel(channelID.String())
	if ch == nil {
		return (&fch{ID: channelID.String()}).Disgo(), nil
	}
	if data.PermissionOverwrites != nil {
		ch.PermissionOverwrites = discordtest.Overwrites(*data.PermissionOverwrites)
	}
	return ch.Disgo(), nil
}

func (f *fakeOps) GetActiveGuildThreads(id snowflake.ID) (*discord.GuildActiveThreads, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threadsAskedFor = id.String()
	if f.threadsErr != nil {
		return nil, f.threadsErr
	}
	out := &discord.GuildActiveThreads{}
	for _, th := range f.threads {
		out.Threads = append(out.Threads, th.Thread())
	}
	return out, nil
}

// GetPublicArchivedThreads pages the way Discord's does: newest archive time
// first, `before` is the cursor, and HasMore says whether another page
// exists. The fake used to hand back the whole slice and ignore both, so
// forumThreads' cursor arithmetic, the part that decides whether a quiet
// entry is found or silently withdrawn, was never exercised.
func (f *fakeOps) GetPublicArchivedThreads(id snowflake.ID, before time.Time, limit int) (*discord.GetThreads, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archivedAskedFor = id.String()
	f.archivedPages++
	if f.archivedErr != nil {
		return nil, f.archivedErr
	}
	if limit <= 0 {
		limit = archivedPageSize
	}

	var rest []*fch
	for _, th := range f.archived {
		if !before.IsZero() && !th.ArchivedAt.Before(before) {
			continue
		}
		rest = append(rest, th)
	}
	page := rest
	if len(page) > limit {
		page = page[:limit]
	}
	out := &discord.GetThreads{HasMore: len(rest) > len(page)}
	for _, th := range page {
		out.Threads = append(out.Threads, th.Thread())
	}
	return out, nil
}

// GetMessage is a thread's starter message: the first message stored under
// it, whatever its ID, as a forum post's starter shares the thread's.
func (f *fakeOps) GetMessage(channelID, _ snowflake.ID) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messageReads++
	msgs := f.messages[channelID.String()]
	if len(msgs) == 0 {
		return nil, &rest.Error{Code: 10008, Message: "Unknown Message", Response: &http.Response{StatusCode: http.StatusNotFound}}
	}
	return &msgs[0], nil
}

func (f *fakeOps) CreateGuildChannel(guildID snowflake.ID, c discord.GuildChannelCreate) (discord.GuildChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFail != nil {
		return nil, f.createFail
	}
	forum, ok := c.(discord.GuildForumChannelCreate)
	if !ok {
		return nil, fmt.Errorf("fakeOps: contest creates forums, got %T", c)
	}
	ch := &fch{
		ID: forumID, Name: forum.Name, GuildID: guildID.String(), Type: forum.Type(), Topic: forum.Topic,
		PermissionOverwrites: discordtest.Overwrites(forum.PermissionOverwrites),
	}
	if forum.ParentID != 0 {
		ch.ParentID = forum.ParentID.String()
	}
	f.created = append(f.created, forum)
	f.channels = append(f.channels, ch)
	return ch.Disgo(), nil
}

// UpdatePermissionOverwrite replaces the whole (allow, deny) pair for one
// target, which is what the real endpoint does and the reason setForumOpen
// has to read before it writes.
func (f *fakeOps) UpdatePermissionOverwrite(channelID, targetID snowflake.ID, u discord.PermissionOverwriteUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, ok := u.(discord.RolePermissionOverwriteUpdate)
	if !ok {
		return fmt.Errorf("fakeOps: contest writes role overwrites, got %T", u)
	}
	var allow, deny discord.Permissions
	if role.Allow != nil {
		allow = *role.Allow
	}
	if role.Deny != nil {
		deny = *role.Deny
	}
	f.overwrit = append(f.overwrit, deny)
	ch := f.channel(channelID.String())
	if ch == nil {
		return nil
	}
	if ow := discordtest.Find(ch.PermissionOverwrites, targetID.String(), discord.PermissionOverwriteTypeRole); ow != nil {
		ow.Allow, ow.Deny = allow, deny
		return nil
	}
	ch.PermissionOverwrites = append(ch.PermissionOverwrites,
		&fow{ID: targetID.String(), Type: discord.PermissionOverwriteTypeRole, Allow: allow, Deny: deny})
	return nil
}

// everyoneOn is the (allow, deny) pair a channel carries for @everyone, which
// in Discord's model is the role whose ID is the guild's.
func (f *fakeOps) everyoneOn(channelID, guildID string) (allow, deny discord.Permissions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch := f.channel(channelID); ch != nil {
		if ow := discordtest.Find(ch.PermissionOverwrites, guildID, discord.PermissionOverwriteTypeRole); ow != nil {
			return ow.Allow, ow.Deny
		}
	}
	return 0, 0
}

func (f *fakeOps) CreateMessage(channelID snowflake.ID, m discord.MessageCreate) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := channelID.String()
	f.sent[id] = append(f.sent[id], m)
	return &discord.Message{ID: 9001, ChannelID: channelID}, nil
}

func (f *fakeOps) PinMessage(channelID, messageID snowflake.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, channelID.String()+"/"+messageID.String())
	return nil
}

func (f *fakeOps) CreateDMChannel(recipientID snowflake.ID) (*discord.DMChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmFails {
		return nil, errors.New("cannot send messages to this user")
	}
	id := dmWith(recipientID.String())
	f.dms[id] = true
	return discordtest.DM(id), nil
}

func strp(s string) *string { return &s }

// dmWith is the DM channel the fake opens with userID.
func dmWith(userID string) string { return "9" + userID }

// sentTo returns every message sent to one channel, so a test can assert
// on what a member would actually have read.
func (f *fakeOps) sentTo(channelID string) []discord.MessageCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]discord.MessageCreate(nil), f.sent[channelID]...)
}

// fakeSched records registrations so the "a job exists only where it has
// work" rule can be asserted directly.
type fakeSched struct {
	mu    sync.Mutex
	jobs  map[string]func(context.Context) error
	seeds []string
}

func newFakeSched() *fakeSched {
	return &fakeSched{jobs: map[string]func(context.Context) error{}}
}

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

func (f *fakeSched) Seed(_ context.Context, key string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeds = append(f.seeds, key)
	return nil
}

func (f *fakeSched) NextDue(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeSched) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.jobs[key]
	return ok
}

// fakeAudit records the durable rows. Tests assert on these to prove a prize
// code never reaches one.
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

// fixedVoice returns the same line every time, so an assertion on an
// announcement is about the code path rather than about which of nine lines
// the RNG picked.
type fixedVoice struct{ line string }

func (v fixedVoice) Line(context.Context, string, voice.Key, map[string]string) string {
	return v.line
}

// newTestPlugin wires the fakes together. workerURL empty means the
// degraded, no-Worker path, which several tests want on purpose.
func newTestPlugin(t interface{ Helper() }, store Store, ops *fakeOps, sched *fakeSched, audit *fakeAudit, workerURL string) *Plugin {
	p := New(store, func(string) DiscordOps { return ops }, fixedVoice{"a line"}, nil,
		workerURL, "bot-token", "link-key")
	p.audit = audit
	p.log = quietLog()
	p.sched = sched
	p.botID = core.ID(botUserID)
	return p
}

// testKey stands in for MERLIN_SECRET_KEY. Generated per process rather than
// written down, for the same reason internal/secret's own test does it: a
// fixed base64 string of exactly the right length is indistinguishable from
// a leaked key to a secret scanner.
func testKey() string {
	buf := make([]byte, secret.KeyBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("contest: generate test key: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// gatedConfig is the minimum a guild has to have said before /contest new
// will do anything: who the forum is for. Tests that are not about the gate
// use it so the refusal does not have to be worked around in each one.
func gatedConfig() Config {
	return Config{GuildID: "1000", DefaultMaxVotes: 2, AccessRoleIDs: []string{"3001"}}
}

// seedGate configures a guild the way the melting pot is: @everyone sees
// nothing, one role is what being let in means. gate-like points at that
// channel, so merlin mirrors it rather than being told a role list.
func seedGate(store *fakeStore, ops *fakeOps) {
	ops.roles = guildRoles("1000", "3001", "3002")
	ops.channels = append(ops.channels, &fch{
		ID: "2005", GuildID: "1000", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{
			{ID: "1000", Type: discord.PermissionOverwriteTypeRole, Deny: discord.PermissionViewChannel},
			{ID: "3001", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel},
		},
	})
	store.cfg["1000"] = Config{GuildID: "1000", DefaultMaxVotes: 2, GateChannelID: "2005"}
}

// dmCount is how many people were sent a direct message. DMs land in `sent`
// under the channel CreateDMChannel handed back, so this is the count of
// those rather than a second recording path.
func (f *fakeOps) dmCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for id := range f.sent {
		if f.dms[id] {
			n++
		}
	}
	return n
}

// roleByID picks one role out of a guildRoles list so a test can give it the
// guild-level permissions the mirror now reads.
func roleByID(roles []discord.Role, id string) *discord.Role {
	for i := range roles {
		if roles[i].ID == core.ID(id) {
			return &roles[i]
		}
	}
	panic("no such role in the fake guild: " + id)
}
