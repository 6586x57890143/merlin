package dragoman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/cygnet/link"

	"github.com/6586x57890143/merlin/internal/core"
)

// Verb is what another bird calls to have a message of its own translated:
// merlin.translate. skua's whispers are the first. The bird posts the
// message itself, under its own webhook, so dragoman never sees it in the
// channel; this is the same translation it would have had, asked for.
//
// cygnet authenticates, merlin authorizes: the asker must run a build its
// CI signed, and everything else is this guild's own dragoman settings,
// read at every call. The body is IDs and the text; merlin posts nothing.
const Verb = "translate"

// maxAsked is the longest text a bird may ask about: one Discord message.
const maxAsked = 2000

// Asked is a translation request's body, each ID a snowflake in decimal.
// Message is the bird's own post of Text, so the audit can link it.
type Asked struct {
	Guild   string `json:"guild"`
	Channel string `json:"channel"`
	Message string `json:"message"`
	User    string `json:"user"`
	Text    string `json:"text"`
}

// Answered is the reply: Text is "" where the message is left as it is,
// Flag the "🇳🇱" of "translated from 🇳🇱", Language its BCP 47 tag.
type Answered struct {
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
	Flag     string `json:"flag,omitempty"`
}

var (
	errAskInvalid = errors.New("dragoman: not a request")
	errAskDenied  = errors.New("dragoman: not for this bird")
	errAskBusy    = errors.New("dragoman: later")
)

// Answer is the link handler: it decodes the body strictly and says why not
// in the link's own codes, nothing more.
func (p *Plugin) Answer(ctx context.Context, c link.Call) (any, error) {
	var a Asked
	dec := json.NewDecoder(bytes.NewReader(c.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || dec.More() {
		return nil, link.Err(link.Invalid, "body")
	}
	out, err := p.TranslateFor(ctx, c.From, c.Trust, a)
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, errAskInvalid):
		return nil, link.Err(link.Invalid, "body")
	case errors.Is(err, errAskDenied):
		return nil, link.Err(link.Denied, "not allowed")
	case errors.Is(err, errAskBusy):
		return nil, link.Err(link.Busy, "later")
	}
	p.log.Error("dragoman: a bird's translation failed", "caller", c.From, "guild", a.Guild, "err", err)
	return nil, err
}

// TranslateFor translates a's text for caller, or answers "" where dragoman
// would have left the same words alone in the channel.
func (p *Plugin) TranslateFor(ctx context.Context, caller string, trust link.Trust, a Asked) (Answered, error) {
	for _, id := range []string{a.Guild, a.Channel, a.Message, a.User} {
		if _, err := snowflake.Parse(id); err != nil || id == "0" {
			return Answered{}, errAskInvalid
		}
	}
	if a.Text == "" || utf8.RuneCountInString(a.Text) > maxAsked {
		return Answered{}, errAskInvalid
	}
	if trust != link.Signed && trust != link.Attested {
		return Answered{}, errAskDenied
	}
	cfg, err := p.config(ctx, core.ID(a.Guild))
	if err != nil {
		return Answered{}, err
	}
	j := job{guildID: a.Guild, channelID: a.Channel, messageID: a.Message, authorID: a.User, content: a.Text, at: p.now()}
	j.evidence = gauge(j.content, p.speaking(j))
	if !cfg.Enabled || !p.gate.PluginEnabled(j.guildID, p.Name()) || slices.Contains(cfg.ExemptChannelIDs, j.channelID) ||
		(cfg.MemberControls && slices.Contains(cfg.OptOutUserIDs, j.authorID)) || j.evidence == none {
		return Answered{}, nil
	}
	if p.held(j.guildID) {
		return Answered{}, errAskBusy
	}
	p.mu.Lock()
	ok := p.allow(j)
	p.mu.Unlock()
	if !ok {
		return Answered{}, errAskBusy
	}
	if j.evidence == weak && p.engine.Unlike(ctx, j.guildID, p.Name(), j.content) {
		p.use(ctx, j.guildID, rungSkipped, 0, nil)
		return Answered{}, nil
	}
	t, err := p.translate(ctx, j)
	if err == nil || errors.Is(err, errNotForTranslating) {
		p.engine.Teach(ctx, j.guildID, p.Name(), j.content, err == nil)
	}
	// Child safety has no harmless version, and that is moderation's.
	if err != nil || t.policy == "child_safety" {
		if err != nil {
			p.failed(j, err)
		}
		return Answered{}, nil
	}
	text, err := p.engine.Publishable(ctx, j.guildID, t.text)
	if err != nil || text == "" {
		return Answered{}, err
	}
	ctx = context.WithoutCancel(ctx)
	p.confirmed(j, languageKey(t.source))
	p.auditFor(ctx, caller, j, t)
	return Answered{Text: text, Language: t.source.String(), Flag: flag(t.source)}, nil
}

// auditFor is auditTranslation for a bird's post: one message, which the
// bird edits in place.
func (p *Plugin) auditFor(ctx context.Context, caller string, j job, t translation) {
	detail := fmt.Sprintf("user=%s channel=%s language=%q", core.MentionUser(j.authorID), core.MentionChannel(j.channelID), languageName(t.source))
	if t.english != "" {
		detail += fmt.Sprintf(" english=%q", t.english)
	}
	detail += fmt.Sprintf(" rung=%q via=%q", t.rung, caller)
	if t.policy != "" {
		detail += fmt.Sprintf(" softened=%q", t.policy)
	}
	detail += fmt.Sprintf(" message=%q", "[Translated]("+core.MessageLink(j.guildID, j.channelID, j.messageID)+")")
	if p.engine.EvidenceHours(ctx, j.guildID) > 0 {
		detail += fmt.Sprintf(" reason=%q", j.content)
	}
	if err := p.audit.Record(ctx, j.guildID, core.ActorSystem, "dragoman.translated", "", detail); err != nil {
		p.log.Error("dragoman: audit failed", "guild", j.guildID, "err", err)
	}
}
