package rotation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeOps is an in-memory DiscordChannelOps used to unit test the rotation
// state machine without a live Discord session. failOnCall lets a test
// inject an error on a specific (1-indexed) call to a named method, so
// step-sequencing and idempotency-on-retry can be tested precisely.
type fakeOps struct {
	mu         sync.Mutex
	channels   map[string]*discordgo.Channel
	messages   map[string][]*discordgo.Message
	nextID     int
	callCounts map[string]int
	failOnCall map[string]int
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		channels:   make(map[string]*discordgo.Channel),
		messages:   make(map[string][]*discordgo.Message),
		callCounts: make(map[string]int),
		failOnCall: make(map[string]int),
		nextID:     1000,
	}
}

func (f *fakeOps) addChannel(ch *discordgo.Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[ch.ID] = ch
}

func (f *fakeOps) shouldFail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCounts[method]++
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
		return nil, fmt.Errorf("fakeOps: channel %s not found", channelID)
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
		return nil, fmt.Errorf("fakeOps: channel %s not found", channelID)
	}
	if data.Name != "" {
		ch.Name = data.Name
	}
	if data.ParentID != "" {
		ch.ParentID = data.ParentID
	}
	if data.PermissionOverwrites != nil {
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
		return nil, fmt.Errorf("fakeOps: channel %s not found", channelID)
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

// fakeArchiveStore is an in-memory ArchiveStore for tests.
type fakeArchiveStore struct {
	mu        sync.Mutex
	records   map[string]ArchiveRecord
	insertErr error
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

func (f *fakeArchiveStore) DueForDeletion(ctx context.Context, guildID string, now time.Time) ([]ArchiveRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ArchiveRecord
	for _, rec := range f.records {
		if rec.GuildID == guildID && rec.DeleteAfter != nil && !rec.DeleteAfter.After(now) {
			out = append(out, rec)
		}
	}
	return out, nil
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

func newTestLoader(t *testing.T, yamlContents string) *config.Loader {
	t.Helper()
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlContents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	l, err := config.NewLoader(path, testLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return l
}

func newTestPlugin(ops *fakeOps, archives ArchiveStore, audit *fakeAudit, cfg *config.Loader, at time.Time) *Plugin {
	// core.NewEventBus wants a logger; the bus itself isn't asserted on in
	// these tests, just exercised so Publish doesn't panic.
	return &Plugin{
		ops:      ops,
		archives: archives,
		cfg:      cfg,
		audit:    audit,
		bus:      core.NewEventBus(testLogger()),
		log:      testLogger(),
		now:      func() time.Time { return at },
	}
}
