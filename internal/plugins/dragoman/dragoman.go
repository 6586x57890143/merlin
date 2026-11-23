// Package dragoman translates messages written in another language into
// English and reposts them as their author, the way aimod reposts a rewrite.
// (A dragoman was the interpreter at a court or an embassy.)
//
// It has no key, no budget and no webhook of its own: the model call and the
// repost are aimod's (Engine), so translation is paid for out of the guild's
// aimod budget and moderated like anything else its author posts. See
// aimod/repost.go for why the swap must never be done here.
package dragoman

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/scripts"
)

// Engine is what dragoman needs of aimod. Satisfied by *aimod.Plugin, wired
// in cmd/bot/main.go.
type Engine interface {
	Ask(ctx context.Context, guildID string, q core.Prompt) (string, error)
	Context(guildID, channelID, messageID, authorID string) (lines []string, self string)
	Policies(ctx context.Context, guildID string) string
	Repost(ctx context.Context, r core.Repost) (string, error)
	OptedOut(ctx context.Context, guildID, userID string) bool
	EvidenceHours(ctx context.Context, guildID string) int
	Budget(ctx context.Context, guildID string) (spent, limit float64, ok bool)
}

// Ops removes a repost for whoever it belongs to. discordguard satisfies it.
type Ops interface {
	DeleteMessage(channelID, messageID snowflake.ID) error
}

// Gate is whether the plugin is on in a guild.
type Gate interface {
	PluginEnabled(guildID, plugin string) bool
}

const (
	// stickyFor is how long a member confirmed speaking a language needs
	// less evidence of it. Long enough to cover a conversation.
	stickyFor = 10 * time.Minute
	// memberMax per memberWindow is one member's share of the budget.
	memberMax    = 20
	memberWindow = 10 * time.Minute
	// queueMax bounds a channel's backlog and staleAfter how old a message
	// may be when its turn comes: past either, replacing it would rewrite a
	// conversation that has moved on.
	queueMax   = 20
	staleAfter = 2 * time.Minute
	// guildParallel is how many channels of one guild are translated at
	// once. aimod has four paid calls at a time; two are left to it.
	guildParallel = 2
	jobTimeout    = 2 * time.Minute
)

// job is one message to translate.
type job struct {
	guildID, channelID, messageID, authorID string
	roleIDs                                 []string
	content, replyTo                        string
	keep                                    bool
	at                                      time.Time
}

type memberKey struct{ guildID, userID string }

type spoke struct {
	language string
	at       time.Time
}

// Plugin implements core.Plugin.
type Plugin struct {
	store   Store
	engine  Engine
	scripts scripts.Store
	gate    Gate
	ops     func(guildID string) Ops
	// held is whether the guild's writes are paused or in dry run: nothing
	// is paid for that could not be posted.
	held     func(guildID string) bool
	scanning bool

	audit    core.AuditWriter
	commands *core.CommandRouter
	bus      *core.EventBus
	log      *slog.Logger
	now      func() time.Time

	cfgMu sync.RWMutex
	cfgs  map[snowflake.ID]Config

	mu      sync.Mutex
	sticky  map[memberKey]spoke
	meter   map[memberKey][]time.Time
	queues  map[string][]job         // channel ID -> waiting
	running map[string]bool          // channel ID -> has a worker
	slots   map[string]chan struct{} // guild ID -> guildParallel
	trouble map[string]string        // guild ID -> why the last one failed

	wg      sync.WaitGroup
	stop    sync.Once
	stopped chan struct{}
}

// New makes the plugin. scanning is whether Discord sends message content;
// without it the commands register and nothing is translated.
func New(store Store, engine Engine, sc scripts.Store, gate Gate, ops func(guildID string) Ops, held func(guildID string) bool, scanning bool) *Plugin {
	return &Plugin{
		store: store, engine: engine, scripts: sc, gate: gate, ops: ops, held: held, scanning: scanning,
		now:     func() time.Time { return time.Now().UTC() },
		cfgs:    map[snowflake.ID]Config{},
		sticky:  map[memberKey]spoke{},
		meter:   map[memberKey][]time.Time{},
		queues:  map[string][]job{},
		running: map[string]bool{},
		slots:   map[string]chan struct{}{},
		trouble: map[string]string{},
		stopped: make(chan struct{}),
	}
}

func (p *Plugin) Name() string { return "dragoman" }

func (p *Plugin) Init(deps core.Deps) error {
	p.audit, p.log, p.commands, p.bus = deps.Audit, deps.Logger, deps.Commands, deps.Bus
	p.registerCommands()
	return nil
}

func (p *Plugin) Start(context.Context) error { return nil }

// Shutdown drops what is waiting and lets a repost under way finish, so a
// restart never lands between the copy and the delete.
func (p *Plugin) Shutdown(ctx context.Context) error {
	p.stop.Do(func() { close(p.stopped) })
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ForgetGuild drops a guild's in-memory state after the bot leaves it.
func (p *Plugin) ForgetGuild(guildID string) {
	p.forgetConfig(guildID)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.sticky {
		if k.guildID == guildID {
			delete(p.sticky, k)
		}
	}
	for k := range p.meter {
		if k.guildID == guildID {
			delete(p.meter, k)
		}
	}
	delete(p.slots, guildID)
	delete(p.trouble, guildID)
}

// config is a guild's settings, read once and kept until one of them is
// changed here. Nothing else writes the table.
func (p *Plugin) config(ctx context.Context, guildID snowflake.ID) (Config, error) {
	p.cfgMu.RLock()
	c, ok := p.cfgs[guildID]
	p.cfgMu.RUnlock()
	if ok {
		return c, nil
	}
	// Held across the read, so a change made meanwhile is not overwritten
	// with what was there before it.
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	c, err := p.store.Config(ctx, guildID.String())
	if err != nil {
		return Config{}, err
	}
	p.cfgs[guildID] = c
	return c, nil
}

func (p *Plugin) forgetConfig(guildID string) {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	delete(p.cfgs, core.ID(guildID))
}

// HandleMessage is the MessageCreate entry point, after aimod's. It decides
// from the message and cached settings alone and returns; the model call
// happens on the channel's own worker. New messages only: an edit is not
// translated, and one made before the repost stops it.
func (p *Plugin) HandleMessage(m discord.Message) {
	if !p.scanning || m.GuildID == nil || m.WebhookID != nil || m.Author.Bot || m.Author.ID == 0 || m.Content == "" {
		return
	}
	if m.Type != discord.MessageTypeDefault && m.Type != discord.MessageTypeReply {
		return
	}
	// A forward's words are somebody else's, and a poll or a voice message
	// is not its text.
	if len(m.MessageSnapshots) > 0 || m.Poll != nil || m.Flags.Has(discord.MessageFlagIsVoiceMessage) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := p.config(ctx, *m.GuildID)
	if err != nil {
		p.log.Error("dragoman: load config", "guild", *m.GuildID, "err", err)
		return
	}
	if !cfg.Enabled {
		return
	}
	j := job{
		guildID: m.GuildID.String(), channelID: m.ChannelID.String(), messageID: m.ID.String(), authorID: m.Author.ID.String(),
		content: m.Content, keep: len(m.Attachments)+len(m.StickerItems) > 0, at: p.now(),
	}
	if slices.Contains(cfg.ExemptChannelIDs, j.channelID) ||
		(cfg.MemberControls && slices.Contains(cfg.OptOutUserIDs, j.authorID)) ||
		!candidate(j.content, p.speaking(j)) || !p.gate.PluginEnabled(j.guildID, p.Name()) {
		return
	}
	if m.Member != nil {
		j.roleIDs = core.IDStrings(m.Member.RoleIDs)
	}
	if m.ReferencedMessage != nil {
		j.replyTo = m.ReferencedMessage.Content
	}
	p.enqueue(j)
}

// speaking is the language j's author was last confirmed writing.
func (p *Plugin) speaking(j job) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sticky[memberKey{j.guildID, j.authorID}]
	if !ok || p.now().Sub(s.at) > stickyFor {
		return ""
	}
	return s.language
}

func (p *Plugin) confirmed(j job, language string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, s := range p.sticky {
		if now.Sub(s.at) > stickyFor {
			delete(p.sticky, k)
		}
	}
	p.sticky[memberKey{j.guildID, j.authorID}] = spoke{language, now}
}

// allow counts one message against its author. Called with p.mu held.
func (p *Plugin) allow(j job) bool {
	now := p.now()
	for k, at := range p.meter {
		if len(at) == 0 || now.Sub(at[len(at)-1]) > memberWindow {
			delete(p.meter, k)
		}
	}
	k := memberKey{j.guildID, j.authorID}
	recent := slices.DeleteFunc(p.meter[k], func(at time.Time) bool { return now.Sub(at) > memberWindow })
	if len(recent) >= memberMax {
		p.meter[k] = recent
		return false
	}
	p.meter[k] = append(recent, now)
	return true
}

// enqueue adds j to its channel, whose worker takes one at a time so
// translations land in the order they were said.
//
// ponytail: one call per message. A channel busier than a message every few
// seconds runs behind and sheds the rest (queueMax, staleAfter); batching a
// run into one call is the upgrade.
func (p *Plugin) enqueue(j job) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.stopped:
		return
	default:
	}
	if len(p.queues[j.channelID]) >= queueMax || !p.allow(j) {
		return
	}
	p.queues[j.channelID] = append(p.queues[j.channelID], j)
	if p.running[j.channelID] {
		return
	}
	p.running[j.channelID] = true
	slots, ok := p.slots[j.guildID]
	if !ok {
		slots = make(chan struct{}, guildParallel)
		p.slots[j.guildID] = slots
	}
	p.wg.Add(1)
	go p.work(j.channelID, slots)
}

// work empties one channel's queue.
func (p *Plugin) work(channelID string, slots chan struct{}) {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		q := p.queues[channelID]
		if len(q) == 0 {
			delete(p.queues, channelID)
			delete(p.running, channelID)
			p.mu.Unlock()
			return
		}
		j := q[0]
		p.queues[channelID] = q[1:]
		p.mu.Unlock()

		select {
		case <-p.stopped:
			return
		case slots <- struct{}{}:
		}
		p.runGuarded(j)
		<-slots
	}
}

// runGuarded keeps a panic on one message from taking the process down.
func (p *Plugin) runGuarded(j job) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("dragoman: panic", "guild", j.guildID, "message", j.messageID, "value", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	p.run(ctx, j)
}

// run translates and reposts one message.
func (p *Plugin) run(ctx context.Context, j job) {
	if p.now().Sub(j.at) > staleAfter || p.held(j.guildID) || p.engine.OptedOut(ctx, j.guildID, j.authorID) {
		return
	}
	t, err := p.translate(ctx, j)
	if err != nil && t.policy == "" {
		p.failed(j, err)
		return
	}
	// With nothing harmless left of it there is nothing to post, and this
	// hands it to moderation and refuses the empty text.
	repostID, err := p.engine.Repost(ctx, core.Repost{
		GuildID: j.guildID, ChannelID: j.channelID, MessageID: j.messageID, AuthorID: j.authorID,
		RoleIDs: j.roleIDs, ReplyTo: j.replyTo,
		Original: j.content, Text: t.text, Marker: marker(t.source), Policy: t.policy, Keep: j.keep,
	})
	if err != nil {
		p.failed(j, err)
		return
	}
	// The message has been replaced: what follows is the record of it, and
	// is not lost to a context that ran out while it was being posted.
	ctx = context.WithoutCancel(ctx)
	p.confirmed(j, baseOf(t.source))
	p.setTrouble(j.guildID, "")
	if err := p.store.Record(ctx, Translation{
		GuildID: j.guildID, ChannelID: j.channelID, RepostID: repostID, AuthorID: j.authorID,
		Language: t.source.String(), CreatedAt: p.now(),
	}); err != nil {
		p.log.Error("dragoman: record translation", "guild", j.guildID, "message", repostID, "err", err)
	}
	p.auditTranslation(ctx, j, t, repostID)
}

// auditTranslation is the log's copy of what a member really wrote. The
// text is left out where the guild keeps no message text at all.
func (p *Plugin) auditTranslation(ctx context.Context, j job, t translation, repostID string) {
	was := "Deleted"
	if j.keep {
		was = "Original"
	}
	detail := fmt.Sprintf("user=%s channel=%s language=%q", core.MentionUser(j.authorID), core.MentionChannel(j.channelID), languageName(t.source))
	if t.english != standard {
		detail += fmt.Sprintf(" english=%q", t.english.name)
	}
	if t.policy != "" {
		detail += fmt.Sprintf(" softened=%q", t.policy)
	}
	detail += fmt.Sprintf(" message=%q", "["+was+"]("+core.MessageLink(j.guildID, j.channelID, j.messageID)+") → [Reposted]("+
		core.MessageLink(j.guildID, j.channelID, repostID)+")")
	if p.engine.EvidenceHours(ctx, j.guildID) > 0 {
		detail += fmt.Sprintf(" reason=%q", j.content)
	}
	if err := p.audit.Record(ctx, j.guildID, core.ActorSystem, "dragoman.translated", "", detail); err != nil {
		p.log.Error("dragoman: audit failed", "guild", j.guildID, "err", err)
	}
}

// failed logs why a message was left alone, and keeps the reasons an admin
// can do something about for the panel.
func (p *Plugin) failed(j job, err error) {
	log := p.log.With("guild", j.guildID, "channel", j.channelID, "message", j.messageID)
	var u interface{ Unavailable() bool }
	switch {
	case errors.Is(err, errLeave), discordguard.Skipped(err):
		log.Debug("dragoman: left alone", "err", err)
	case errors.As(err, &u) && u.Unavailable():
		log.Info("dragoman: not translated", "err", err)
		p.setTrouble(j.guildID, err.Error())
	default:
		log.Error("dragoman: translation failed", "err", err)
		p.setTrouble(j.guildID, err.Error())
	}
}

func (p *Plugin) setTrouble(guildID, why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if why == "" {
		delete(p.trouble, guildID)
		return
	}
	p.trouble[guildID] = why
}

// Perms is what dragoman's reposts need where it is on.
func (p *Plugin) Perms(guildID string) discord.Permissions {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := p.config(ctx, core.ID(guildID))
	if err != nil || !cfg.Enabled {
		return 0
	}
	return discord.PermissionViewChannel | discord.PermissionReadMessageHistory |
		discord.PermissionManageMessages | discord.PermissionManageWebhooks
}
