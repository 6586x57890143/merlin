package contest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/secret"
	"github.com/6586x57890143/merlin/internal/voice"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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
	liveErr error
	subsErr error
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

func (f *fakeStore) GuildsWithLiveContests(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.contests {
		if c.Live() {
			out = append(out, c.GuildID)
		}
	}
	return out, nil
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
			list[i] = s
			f.subs[s.ContestID] = list
			return nil
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

	channels []*discordgo.Channel
	threads  []*discordgo.Channel
	messages map[string][]*discordgo.Message // channel/thread ID -> messages

	created  []discordgo.GuildChannelCreateData
	sent     map[string][]*discordgo.MessageSend
	overwrit []int64 // deny masks passed to ChannelPermissionSet, in order
	pinned   []string

	dmFails    bool
	createFail error
	threadsErr error
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		messages: map[string][]*discordgo.Message{},
		sent:     map[string][]*discordgo.MessageSend{},
	}
}

func (f *fakeOps) Channel(id string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: id}, nil
}

func (f *fakeOps) GuildChannels(string, ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.channels, nil
}

func (f *fakeOps) ThreadsActive(string, ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.threadsErr != nil {
		return nil, f.threadsErr
	}
	return &discordgo.ThreadsList{Threads: f.threads}, nil
}

func (f *fakeOps) ChannelMessages(channelID string, _ int, _, _, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[channelID], nil
}

func (f *fakeOps) GuildChannelCreateComplex(_ string, data discordgo.GuildChannelCreateData, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFail != nil {
		return nil, f.createFail
	}
	f.created = append(f.created, data)
	return &discordgo.Channel{ID: "forum-1", Name: data.Name}, nil
}

func (f *fakeOps) ChannelPermissionSet(_, _ string, _ discordgo.PermissionOverwriteType, _, deny int64, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overwrit = append(f.overwrit, deny)
	return nil
}

func (f *fakeOps) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[channelID] = append(f.sent[channelID], data)
	return &discordgo.Message{ID: "msg-1", ChannelID: channelID}, nil
}

func (f *fakeOps) ChannelMessagePin(channelID, messageID string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, channelID+"/"+messageID)
	return nil
}

func (f *fakeOps) UserChannelCreate(recipientID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmFails {
		return nil, errors.New("cannot send messages to this user")
	}
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}

// sentTo returns every message body sent to one channel, flattened, so a
// test can assert on what a member would actually have read.
func (f *fakeOps) sentTo(channelID string) []*discordgo.MessageSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*discordgo.MessageSend(nil), f.sent[channelID]...)
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
