package rotation

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/json/v2"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/voice"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// findOtherChannelByName locates a channel by name, ignoring excludeID,
// a test-side assertion helper for "did the replacement channel end up
// where it should", deliberately looser than rotate's own replacement
// matching so a test can catch a replacement that landed in the wrong
// category instead of silently failing to find it.
func findOtherChannelByName(channels []*fch, name, excludeID string) *fch {
	for _, c := range channels {
		if c.Name == name && c.ID != excludeID {
			return c
		}
	}
	return nil
}

// fch is a channel as the fake holds it, with string IDs so fixtures read
// as the settings they sit beside. disgo's channels are immutable values
// with unexported fields, so the fake keeps its own and builds disgo's
// from them on the way out, through the same JSON Discord sends.
type fch struct {
	ID, GuildID, ParentID, Name, Topic string
	Type                               discord.ChannelType
	Position, RateLimitPerUser         int
	NSFW                               bool
	PermissionOverwrites               []*fow
}

// fow is one permission overwrite, string-ID'd like fch.
type fow struct {
	ID          string
	Type        discord.PermissionOverwriteType
	Allow, Deny discord.Permissions
}

func (c *fch) disgo() discord.GuildChannel {
	ows := make([]overwrite, len(c.PermissionOverwrites))
	for i, o := range c.PermissionOverwrites {
		ows[i] = overwrite{Type: o.Type, ID: core.ID(o.ID), Allow: o.Allow, Deny: o.Deny}
	}
	raw := map[string]any{
		"name": c.Name, "type": c.Type, "position": c.Position,
		"topic": c.Topic, "nsfw": c.NSFW, "rate_limit_per_user": c.RateLimitPerUser,
		"permission_overwrites": disgoOverwrites(ows),
	}
	for k, v := range map[string]string{"id": c.ID, "guild_id": c.GuildID, "parent_id": c.ParentID} {
		if v != "" {
			raw[k] = v
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	var u discord.UnmarshalChannel
	if err := json.Unmarshal(b, &u); err != nil {
		panic(err)
	}
	return u.Channel.(discord.GuildChannel)
}

func fowsOf(ows []discord.PermissionOverwrite) []*fow {
	out := make([]*fow, 0, len(ows))
	for _, o := range overwritesOf(ows) {
		out = append(out, &fow{ID: o.ID.String(), Type: o.Type, Allow: o.Allow, Deny: o.Deny})
	}
	return out
}

// fmsg is a message the fake holds.
type fmsg struct{ ID, ChannelID, Content string }

// sentEmbed is a message the fake was sent with an embed: the embed and the
// files that travelled with it.
type sentEmbed struct {
	Embed *discord.Embed
	Files []*discord.File
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
	channels   map[string]*fch
	messages   map[string][]*fmsg
	nextID     int
	callCounts map[string]int
	failOnCall map[string]int
	failWith   map[string]error

	// editCalls records every UpdateChannel in order. Final state alone
	// can't express the position restore's correctness argument, which is
	// about *when* it happens relative to the old channel leaving the
	// category; an assertion on the end state would pass even if the two
	// were swapped.
	editCalls []recordedEdit

	// complexSends records every message sent with an embed, so a test can
	// assert on the embed and its attachments rather than only on the fact
	// that something was posted.
	complexSends []sentEmbed

	// threads is what GetActiveGuildThreads returns. Guild-scoped, so it
	// deliberately holds threads from more than one parent: the filtering is
	// the part worth testing.
	threads []*fch
}

type recordedEdit struct {
	channelID string
	name      string
	parentID  string
	position  *int
}

// Fixture IDs. Snowflakes, because the plugin parses what it stores.
const (
	testArchiveCategoryID = "200"
	testBotID             = "800"
)

func newFakeOps() *fakeOps {
	f := &fakeOps{
		channels:   make(map[string]*fch),
		messages:   make(map[string][]*fmsg),
		callCounts: make(map[string]int),
		failOnCall: make(map[string]int),
		failWith:   make(map[string]error),
		nextID:     1000,
	}
	// testArchiveCategoryID is pre-registered because a real guild always
	// has one: rotate() reconciles the category's permissions before
	// archiving into it (archiveperms.go), so a fake guild where the
	// category simply does not exist is not a case worth making every test
	// set up by hand.
	f.channels[testArchiveCategoryID] = &fch{
		ID:   testArchiveCategoryID,
		Name: defaultArchiveCategoryName,
		Type: discord.ChannelTypeGuildCategory,
	}
	return f
}

func (f *fakeOps) addChannel(ch *fch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[ch.ID] = ch
}

// restErr is the error disgo returns for a Discord error response.
func restErr(status int, code rest.JSONErrorCode, msg string) error {
	return &rest.Error{Code: code, Message: msg, Response: &http.Response{StatusCode: status}}
}

// unknownChannelErr is what Discord answers for a channel that no longer
// exists: its own 10003 code, rather than a bare error string. Callers
// distinguish that case from a transient failure (core.IsUnknownResource),
// so a fake that returned an undifferentiated error would let "gone" and
// "try again later" test identically, which is precisely the bug that
// distinction exists to catch.
func unknownChannelErr(channelID string) error {
	return restErr(http.StatusNotFound, rest.JSONErrorCode(10003), "Unknown Channel: "+channelID)
}

// transientErr is a retryable Discord failure (a 500), the counterpart to
// unknownChannelErr.
func transientErr() error {
	return restErr(http.StatusInternalServerError, 0, "500: Internal Server Error")
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

// Channel is the test's own read of a channel: a copy, never failed or
// counted.
func (f *fakeOps) Channel(channelID string) (*fch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[channelID]
	if !ok {
		return nil, unknownChannelErr(channelID)
	}
	cp := *ch
	return &cp, nil
}

// GuildChannels is the test's own read of a guild's channels.
func (f *fakeOps) GuildChannels(guildID string) ([]*fch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fch
	for _, ch := range f.channels {
		if ch.GuildID == guildID {
			cp := *ch
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeOps) GetChannel(channelID snowflake.ID) (discord.Channel, error) {
	if err := f.shouldFail("GetChannel"); err != nil {
		return nil, err
	}
	ch, err := f.Channel(channelID.String())
	if err != nil {
		return nil, err
	}
	return ch.disgo(), nil
}

func (f *fakeOps) GetGuildChannels(guildID snowflake.ID) ([]discord.GuildChannel, error) {
	if err := f.shouldFail("GetGuildChannels"); err != nil {
		return nil, err
	}
	chans, _ := f.GuildChannels(guildID.String())
	out := make([]discord.GuildChannel, len(chans))
	for i, c := range chans {
		out[i] = c.disgo()
	}
	return out, nil
}

func (f *fakeOps) GetActiveGuildThreads(snowflake.ID) (*discord.GuildActiveThreads, error) {
	if err := f.shouldFail("GetActiveGuildThreads"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &discord.GuildActiveThreads{}
	for _, t := range f.threads {
		cp := *t
		cp.Type = discord.ChannelTypeGuildPublicThread
		out.Threads = append(out.Threads, cp.disgo().(discord.GuildThread))
	}
	return out, nil
}

func (f *fakeOps) CreateGuildChannel(guildID snowflake.ID, c discord.GuildChannelCreate) (discord.GuildChannel, error) {
	if err := f.shouldFail("CreateGuildChannel"); err != nil {
		return nil, err
	}
	var text discord.GuildTextChannelCreate
	switch c := c.(type) {
	case discord.GuildTextChannelCreate:
		text = c
	case discord.GuildNewsChannelCreate:
		text = discord.GuildTextChannelCreate(c)
	case discord.GuildCategoryChannelCreate:
		text = discord.GuildTextChannelCreate{Name: c.Name, Position: c.Position, PermissionOverwrites: c.PermissionOverwrites}
	default:
		return nil, fmt.Errorf("fakeOps: cannot create a %T", c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	ch := &fch{
		ID:                   strconv.Itoa(f.nextID),
		GuildID:              guildID.String(),
		Name:                 text.Name,
		Type:                 c.Type(),
		Topic:                text.Topic,
		RateLimitPerUser:     text.RateLimitPerUser,
		Position:             text.Position,
		PermissionOverwrites: fowsOf(text.PermissionOverwrites),
		NSFW:                 text.NSFW,
	}
	if text.ParentID != 0 {
		ch.ParentID = text.ParentID.String()
	}
	f.channels[ch.ID] = ch
	return ch.disgo(), nil
}

func (f *fakeOps) UpdateChannel(channelID snowflake.ID, u discord.ChannelUpdate) (discord.Channel, error) {
	if err := f.shouldFail("UpdateChannel"); err != nil {
		return nil, err
	}
	data, ok := u.(discord.GuildTextChannelUpdate)
	if !ok {
		return nil, fmt.Errorf("fakeOps: rotation sends GuildTextChannelUpdate, got %T", u)
	}
	id := channelID.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := recordedEdit{channelID: id, position: data.Position}
	if data.Name != nil {
		rec.name = *data.Name
	}
	if data.ParentID != nil {
		rec.parentID = data.ParentID.String()
	}
	f.editCalls = append(f.editCalls, rec)
	ch, ok := f.channels[id]
	if !ok {
		return nil, unknownChannelErr(id)
	}
	if rec.name != "" {
		ch.Name = rec.name
	}
	if rec.parentID != "" {
		ch.ParentID = rec.parentID
	}
	// As Discord does: no list leaves the overwrites alone, and any list,
	// an empty one included, replaces them.
	if data.PermissionOverwrites != nil {
		ch.PermissionOverwrites = fowsOf(*data.PermissionOverwrites)
	}
	// Position is a *int precisely so "move to 0" is distinguishable from
	// "leave it alone"; reproducing that here is what lets a test catch a
	// rotation that puts the replacement at the top of the category.
	if data.Position != nil {
		ch.Position = *data.Position
	}
	return ch.disgo(), nil
}

func (f *fakeOps) DeleteChannel(channelID snowflake.ID) error {
	if err := f.shouldFail("DeleteChannel"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.channels[channelID.String()]; !ok {
		return unknownChannelErr(channelID.String())
	}
	delete(f.channels, channelID.String())
	return nil
}

// ChannelMessages is the test's own read of up to limit of a channel's
// messages.
func (f *fakeOps) ChannelMessages(channelID string, limit int) []*fmsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.messages[channelID]
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}
	return msgs
}

func (f *fakeOps) GetMessages(channelID, _ snowflake.ID, limit int) ([]discord.Message, error) {
	if err := f.shouldFail("GetMessages"); err != nil {
		return nil, err
	}
	msgs := f.ChannelMessages(channelID.String(), limit)
	out := make([]discord.Message, len(msgs))
	for i, m := range msgs {
		out[i] = discord.Message{ID: core.ID(m.ID), ChannelID: channelID, Content: m.Content}
	}
	return out, nil
}

// CreateMessage records an embed's whole payload, not just that a send
// happened, so a test can check the retention notice actually carries the
// file its thumbnail references.
func (f *fakeOps) CreateMessage(channelID snowflake.ID, m discord.MessageCreate) (*discord.Message, error) {
	if err := f.shouldFail("CreateMessage"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(m.Embeds) > 0 {
		f.complexSends = append(f.complexSends, sentEmbed{Embed: &m.Embeds[0], Files: m.Files})
	}
	f.nextID++
	msg := &fmsg{ID: strconv.Itoa(f.nextID), ChannelID: channelID.String(), Content: m.Content}
	f.messages[msg.ChannelID] = append(f.messages[msg.ChannelID], msg)
	return &discord.Message{ID: core.ID(msg.ID), ChannelID: channelID, Content: m.Content}, nil
}

func (f *fakeOps) PinMessage(_, _ snowflake.ID) error {
	return f.shouldFail("PinMessage")
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

// fakeSettings is an in-memory SettingsProvider for tests, mirroring
// fakeArchiveStore's role, standing in for internal/settings.Store without
// a live Postgres.
type fakeSettings struct {
	mu            sync.Mutex
	modRoles      map[string][]string
	archiveViewer map[string][]string
	rotations     map[string]map[string]settings.RotationChannel // guildID -> channelID -> config
	nextID        int64
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{
		modRoles:      make(map[string][]string),
		archiveViewer: make(map[string][]string),
		rotations:     make(map[string]map[string]settings.RotationChannel),
	}
}

func (f *fakeSettings) ModRoleIDs(guildID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modRoles[guildID]
}

func (f *fakeSettings) ArchiveViewerRoleIDs(guildID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.archiveViewer[guildID]
}

func (f *fakeSettings) AddArchiveViewerRole(_ context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.archiveViewer[guildID], roleID) {
		f.archiveViewer[guildID] = append(f.archiveViewer[guildID], roleID)
	}
	return nil
}

func (f *fakeSettings) RemoveArchiveViewerRole(_ context.Context, guildID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archiveViewer[guildID] = slices.DeleteFunc(f.archiveViewer[guildID], func(r string) bool { return r == roleID })
	return nil
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
// key stability directly: records how many times each key was registered
// or unregistered, so a test can assert a job was registered exactly once
// despite its underlying settings row's ChannelID changing underneath it.
type fakeScheduler struct {
	mu              sync.Mutex
	registered      map[string]bool
	registerCalls   map[string]int
	unregisterCalls map[string]int
	seedCalls       map[string]time.Time

	// nextDue is what NextDue answers per job key. Absent means "no future
	// instant", which is how both an unregistered job and an already-overdue
	// one look to a caller.
	nextDue    map[string]time.Time
	nextDueErr error
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{
		registered:      make(map[string]bool),
		registerCalls:   make(map[string]int),
		unregisterCalls: make(map[string]int),
		nextDue:         make(map[string]time.Time),
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

func (f *fakeScheduler) NextDue(ctx context.Context, jobKey string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextDueErr != nil {
		return time.Time{}, false, f.nextDueErr
	}
	due, ok := f.nextDue[jobKey]
	return due, ok, nil
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
		voice:    testVoice(),
		botID:    core.ID(testBotID),
	}
}

// testVoice is the real catalog, not a stub. Rotation's tests assert on the
// text members actually see, so substituting fixed strings here would test
// the plumbing while leaving the thing that reaches the server unchecked.
// It also means a catalog that stops loading fails these tests too, not
// only the voice package's own.
func testVoice() voice.Source {
	sp, err := voice.New(testLogger())
	if err != nil {
		panic("voice catalog does not load: " + err.Error())
	}
	return sp
}
