package rotation

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// findOtherChannelByName locates a channel by name, ignoring excludeID —
// a test-side assertion helper for "did the replacement channel end up
// where it should", deliberately looser than rotate's own replacement
// matching so a test can catch a replacement that landed in the wrong
// category instead of silently failing to find it.
func findOtherChannelByName(channels []*discordgo.Channel, name, excludeID string) *discordgo.Channel {
	return findChannel(channels, func(c *discordgo.Channel) bool { return c.Name == name && c.ID != excludeID })
}

// fakeOps is an in-memory DiscordChannelOps used to unit test the rotation
// state machine without a live Discord session. failOnCall lets a test
// inject an error on a specific (1-indexed) call to a named method, so
// step-sequencing and idempotency-on-retry can be tested precisely.
// failWith is failOnCall's counterpart for tests that care about the *kind*
// of failure rather than which call fails: it fails every call to a named
// method with a specific error, until the test removes the entry.
type fakeOps struct {
	mu         sync.Mutex
	channels   map[string]*discordgo.Channel
	messages   map[string][]*discordgo.Message
	nextID     int
	callCounts map[string]int
	failOnCall map[string]int
	failWith   map[string]error
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		channels:   make(map[string]*discordgo.Channel),
		messages:   make(map[string][]*discordgo.Message),
		callCounts: make(map[string]int),
		failOnCall: make(map[string]int),
		failWith:   make(map[string]error),
		nextID:     1000,
	}
}

func (f *fakeOps) addChannel(ch *discordgo.Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[ch.ID] = ch
}

// unknownChannelErr mirrors what discordgo returns for a channel that no
// longer exists — a *discordgo.RESTError carrying Discord's own 10003 code —
// rather than a bare error string. Callers distinguish that case from a
// transient failure (core.IsUnknownResource), so a fake that returned an
// undifferentiated error would let "gone" and "try again later" test
// identically, which is precisely the bug that distinction exists to catch.
func unknownChannelErr(channelID string) error {
	return &discordgo.RESTError{
		Response:     &http.Response{StatusCode: http.StatusNotFound},
		ResponseBody: []byte(`{"code":10003,"message":"Unknown Channel"}`),
		Message:      &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownChannel, Message: "Unknown Channel: " + channelID},
	}
}

// transientErr is a retryable Discord failure (a 500), the counterpart to
// unknownChannelErr.
func transientErr() error {
	return &discordgo.RESTError{
		Response:     &http.Response{StatusCode: http.StatusInternalServerError},
		ResponseBody: []byte(`{"code":0,"message":"500: Internal Server Error"}`),
		Message:      &discordgo.APIErrorMessage{Code: 0, Message: "500: Internal Server Error"},
	}
}

func (f *fakeOps) shouldFail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCounts[method]++
	if err, ok := f.failWith[method]; ok {
		return err
	}
	if n, ok := f.failOnCall[method]; ok && f.callCounts[method] == n {
		return fmt.Errorf("injected failure: %s call #%d", method, n)
	}
	return nil
}

func (f *fakeOps) Channel(channelID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := f.shouldFail("Channel"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[channelID]
	if !ok {
		return nil, unknownChannelErr(channelID)
	}
	cp := *ch
	return &cp, nil
}

func (f *fakeOps) GuildChannels(guildID string, _ ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	if err := f.shouldFail("GuildChannels"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*discordgo.Channel
	for _, ch := range f.channels {
		if ch.GuildID == guildID {
			cp := *ch
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeOps) ThreadsActive(channelID string, _ ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
	if err := f.shouldFail("ThreadsActive"); err != nil {
		return nil, err
	}
	return &discordgo.ThreadsList{}, nil
}

func (f *fakeOps) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := f.shouldFail("GuildChannelCreateComplex"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	ch := &discordgo.Channel{
		ID:                   fmt.Sprintf("ch%d", f.nextID),
		GuildID:              guildID,
		Name:                 data.Name,
		Type:                 data.Type,
		Topic:                data.Topic,
		RateLimitPerUser:     data.RateLimitPerUser,
		Position:             data.Position,
		PermissionOverwrites: data.PermissionOverwrites,
		ParentID:             data.ParentID,
		NSFW:                 data.NSFW,
	}
	f.channels[ch.ID] = ch
	return ch, nil
}

func (f *fakeOps) ChannelEditComplex(channelID string, data *discordgo.ChannelEdit, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := f.shouldFail("ChannelEditComplex"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[channelID]
	if !ok {
		return nil, unknownChannelErr(channelID)
	}
	if data.Name != "" {
		ch.Name = data.Name
	}
	if data.ParentID != "" {
		ch.ParentID = data.ParentID
	}
	if len(data.PermissionOverwrites) > 0 {
		// Mirrors discordgo's real wire behavior: ChannelEdit.PermissionOverwrites
		// is `json:"...,omitempty"`, so a nil OR empty slice is dropped from the
		// outgoing PATCH entirely — Discord then leaves existing overwrites
		// untouched. A `!= nil` check here (an empty-but-non-nil slice) would
		// let a test believe an explicit-clear succeeded when the real API
		// would silently no-op it — exactly the bug that shipped in
		// revealNewChannel because this fake didn't reproduce it.
		ch.PermissionOverwrites = data.PermissionOverwrites
	}
	return ch, nil
}

func (f *fakeOps) ChannelDelete(channelID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := f.shouldFail("ChannelDelete"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[channelID]
	if !ok {
		return nil, unknownChannelErr(channelID)
	}
	delete(f.channels, channelID)
	return ch, nil
}

func (f *fakeOps) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	if err := f.shouldFail("ChannelMessages"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.messages[channelID]
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}
	return msgs, nil
}

func (f *fakeOps) appendMessage(channelID, content string) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	msg := &discordgo.Message{ID: fmt.Sprintf("msg%d", f.nextID), ChannelID: channelID, Content: content}
	f.messages[channelID] = append(f.messages[channelID], msg)
	return msg, nil
}

func (f *fakeOps) ChannelMessageSend(channelID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if err := f.shouldFail("ChannelMessageSend"); err != nil {
		return nil, err
	}
	return f.appendMessage(channelID, content)
}

func (f *fakeOps) ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if err := f.shouldFail("ChannelMessageSendEmbed"); err != nil {
		return nil, err
	}
	return f.appendMessage(channelID, "")
}

func (f *fakeOps) ChannelMessagePin(channelID, messageID string, _ ...discordgo.RequestOption) error {
	return f.shouldFail("ChannelMessagePin")
}

func (f *fakeOps) User(userID string, _ ...discordgo.RequestOption) (*discordgo.User, error) {
	if err := f.shouldFail("User"); err != nil {
		return nil, err
	}
	return &discordgo.User{ID: "bot-user-id"}, nil
}

// fakeArchiveStore is an in-memory ArchiveStore for tests.
type fakeArchiveStore struct {
	mu        sync.Mutex
	records   map[string]ArchiveRecord
	insertErr error
	listErr   error
}

func newFakeArchiveStore() *fakeArchiveStore {
	return &fakeArchiveStore{records: make(map[string]ArchiveRecord)}
}

func (f *fakeArchiveStore) Insert(ctx context.Context, rec ArchiveRecord) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[rec.ChannelID] = rec
	return nil
}

func (f *fakeArchiveStore) ListForGuild(ctx context.Context, guildID string) ([]ArchiveRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []ArchiveRecord
	for _, rec := range f.records {
		if rec.GuildID == guildID {
			out = append(out, rec)
		}
	}
	// Map iteration order is random; sweep results are asserted per-record,
	// but a stable order keeps failures readable.
	slices.SortFunc(out, func(a, b ArchiveRecord) int { return cmp.Compare(a.ChannelID, b.ChannelID) })
	return out, nil
}

// dueForDeletion is the test-only convenience the production store no longer
// provides: due-ness now depends on the live retention setting, so it's
// decided in sweep.go rather than in a query. Tests that only care about
// "which rows would this sweep act on" use this against a nil lookup, i.e.
// the stored-deadline fallback.
func (f *fakeArchiveStore) dueForDeletion(guildID string, now time.Time) []ArchiveRecord {
	all, _ := f.ListForGuild(context.Background(), guildID)
	var out []ArchiveRecord
	for _, rec := range all {
		if rec.DeleteAfter != nil && !rec.DeleteAfter.After(now) {
			out = append(out, rec)
		}
	}
	return out
}

func (f *fakeArchiveStore) Delete(ctx context.Context, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, channelID)
	return nil
}

type auditRecord struct {
	guildID, actorID, action, oldValue, newValue string
}

// fakeAudit is an in-memory core.AuditWriter for tests.
type fakeAudit struct {
	mu      sync.Mutex
	records []auditRecord
	failErr error
}

func (f *fakeAudit) Record(ctx context.Context, guildID, actorID, action, oldValue, newValue string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, auditRecord{guildID, actorID, action, oldValue, newValue})
	return nil
}

// fakeSettings is an in-memory SettingsProvider for tests — mirrors
// fakeArchiveStore's role, standing in for internal/settings.Store without
// a live Postgres.
type fakeSettings struct {
	mu        sync.Mutex
	modRoles  map[string][]string
	rotations map[string]map[string]settings.RotationChannel // guildID -> channelID -> config
	nextID    int64
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{
		modRoles:  make(map[string][]string),
		rotations: make(map[string]map[string]settings.RotationChannel),
	}
}

func (f *fakeSettings) ModRoleIDs(guildID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modRoles[guildID]
}

func (f *fakeSettings) RotationChannels(guildID string) []settings.RotationChannel {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]settings.RotationChannel, 0, len(f.rotations[guildID]))
	for _, rc := range f.rotations[guildID] {
		out = append(out, rc)
	}
	return out
}

func (f *fakeSettings) RotationChannel(guildID, channelID string) (settings.RotationChannel, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc, ok := f.rotations[guildID][channelID]
	return rc, ok
}

func (f *fakeSettings) RotationChannelByID(guildID string, id int64) (settings.RotationChannel, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rc := range f.rotations[guildID] {
		if rc.ID == id {
			return rc, true
		}
	}
	return settings.RotationChannel{}, false
}

func (f *fakeSettings) UpsertRotationChannel(ctx context.Context, rc settings.RotationChannel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rotations[rc.GuildID] == nil {
		f.rotations[rc.GuildID] = make(map[string]settings.RotationChannel)
	}
	if rc.ID == 0 {
		// Mirrors the real Store's BIGSERIAL id column: assigned once, on
		// first insert, and never touched again by an update-in-place.
		f.nextID++
		rc.ID = f.nextID
	}
	f.rotations[rc.GuildID][rc.ChannelID] = rc
	return nil
}

func (f *fakeSettings) RemoveRotationChannel(ctx context.Context, guildID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rotations[guildID], channelID)
	return nil
}

func (f *fakeSettings) RetargetRotationChannel(ctx context.Context, guildID, oldChannelID, newChannelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc, ok := f.rotations[guildID][oldChannelID]
	if !ok {
		return nil
	}
	rc.ChannelID = newChannelID
	delete(f.rotations[guildID], oldChannelID)
	f.rotations[guildID][newChannelID] = rc
	return nil
}

// fakeScheduler is an in-memory core.Scheduler for testing reconcile's job
// key stability directly — records how many times each key was registered
// or unregistered, so a test can assert a job was registered exactly once
// despite its underlying settings row's ChannelID changing underneath it.
type fakeScheduler struct {
	mu              sync.Mutex
	registered      map[string]bool
	registerCalls   map[string]int
	unregisterCalls map[string]int
	seedCalls       map[string]time.Time
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{
		registered:      make(map[string]bool),
		registerCalls:   make(map[string]int),
		unregisterCalls: make(map[string]int),
		seedCalls:       make(map[string]time.Time),
	}
}

func (f *fakeScheduler) Register(jobKey string, spec core.CronSpec, fn func(ctx context.Context) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registered[jobKey] {
		return fmt.Errorf("fakeScheduler: job %q already registered", jobKey)
	}
	f.registered[jobKey] = true
	f.registerCalls[jobKey]++
	return nil
}

func (f *fakeScheduler) Unregister(jobKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.registered, jobKey)
	f.unregisterCalls[jobKey]++
	return nil
}

func (f *fakeScheduler) RunNow(ctx context.Context, jobKey string) error {
	return fmt.Errorf("fakeScheduler: RunNow not supported in this test double")
}

func (f *fakeScheduler) Seed(ctx context.Context, jobKey string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls[jobKey] = at
	return nil
}

func newTestPlugin(ops *fakeOps, archives ArchiveStore, audit *fakeAudit, fs *fakeSettings, at time.Time) *Plugin {
	// core.NewEventBus wants a logger; the bus itself isn't asserted on in
	// these tests, just exercised so Publish doesn't panic.
	return &Plugin{
		ops:      func(string) DiscordChannelOps { return ops },
		dryRun:   func(string) bool { return false },
		archives: archives,
		settings: fs,
		audit:    audit,
		bus:      core.NewEventBus(testLogger()),
		log:      testLogger(),
		now:      func() time.Time { return at },
	}
}
