package core

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Ix is one interaction as a plugin on disgo sees it: disgo's own decoding
// of what Discord sent, the REST client its answer goes through, and the
// guild and member as the string IDs merlin's settings are keyed by.
//
// It is how plugins move off discordgo one at a time (spec.MD, the disgo
// port). The router still reads discordgo's copy of the same interaction
// for the plugin gate and Authorize, so neither check moves while plugins
// do; a handler registered with HandleIx, HandleComponentIx or
// HandleModalIx gets an Ix instead of a session and an InteractionCreate.
// The response methods below are the discordgo helpers' counterparts, with
// the same flags, the same attachments and the same no-pings rule.
type Ix struct {
	I discord.Interaction
	// Rest answers the interaction. Every response call is authorised by the
	// interaction's own token, so it shares no rate limit with anything the
	// bot does as itself.
	Rest rest.Rest
	// GuildID is "" outside a server; the router refuses those first.
	GuildID string
	// UserID is whoever the interaction is from, and ChannelID the channel
	// it came from.
	UserID, ChannelID string
}

// errNoPayload is an Ix handler reached without the raw interaction it is
// decoded from, which only a test calling HandleInteraction can do.
var errNoPayload = errors.New("core: no raw interaction payload for an Ix handler")

// NewIx decodes raw, an INTERACTION_CREATE's data, for a plugin on disgo.
func NewIx(raw []byte, r rest.Rest) (*Ix, error) {
	if raw == nil {
		return nil, errNoPayload
	}
	if r == nil {
		return nil, errors.New("core: no REST client for an Ix handler (CommandRouter.WithRest)")
	}
	i, err := discord.UnmarshalInteraction(raw)
	if err != nil {
		return nil, fmt.Errorf("core: decoding the interaction: %w", err)
	}
	ix := &Ix{I: i, Rest: r, UserID: i.User().ID.String()}
	// The channel is optional in the payload, and disgo's wraps an interface
	// that is nil without it: read it only when it is there, since a panic
	// here is outside every handler's recover.
	if c := i.Channel(); c.MessageChannel != nil {
		ix.ChannelID = c.ID().String()
	}
	if g := i.GuildID(); g != nil {
		ix.GuildID = g.String()
	}
	return ix, nil
}

// Member is the invoking member, nil outside a server.
func (ix *Ix) Member() *discord.ResolvedMember { return ix.I.Member() }

// noPings is the AllowedMentions every response carries: nothing parsed,
// nobody listed. The REST client applies the same by default
// (WithDefaultAllowedMentions in NewRest); this is stated per call so a
// response built somewhere else still cannot ping.
func noPings() *discord.AllowedMentions {
	return &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}}
}

// NoPings is noPings for a plugin building its own message.
func NoPings() *discord.AllowedMentions { return noPings() }

// NewRest is the REST client the router hands every Ix: the bot's token,
// mentions off unless a call says otherwise, and client, which is the
// session's own so every interaction reply is timed for /perf and reported
// to cygnet when refused, as every other request is. A nil client is
// disgo's default.
func NewRest(token string, client *http.Client) rest.Rest {
	var opts []rest.ClientConfigOpt
	if client != nil {
		opts = append(opts, rest.WithHTTPClient(client))
	}
	return rest.New(rest.NewClient(token, opts...), rest.WithDefaultAllowedMentions(*noPings()))
}

func (ix *Ix) respond(t discord.InteractionResponseType, data discord.InteractionResponseData) error {
	return ix.Rest.CreateInteractionResponse(ix.I.ID(), ix.I.Token(), discord.InteractionResponse{Type: t, Data: data})
}

func (ix *Ix) edit(m discord.MessageUpdate) error {
	_, err := ix.Rest.UpdateInteractionResponse(ix.I.ApplicationID(), ix.I.Token(), m)
	return err
}

// noAttachments is an empty, not omitted, attachment list: an edit that
// omits it keeps what is on the message and appends the new uploads, until
// Discord's cap of ten refuses the eleventh (see followUpFiles).
func noAttachments() *[]discord.AttachmentUpdate { return &[]discord.AttachmentUpdate{} }

// RespondEmbed is RespondEmbed: embed, privately, with what it references.
func (ix *Ix) RespondEmbed(embed discord.Embed) error {
	return ix.respond(discord.InteractionResponseTypeCreateMessage, discord.MessageCreate{
		Embeds: []discord.Embed{embed}, Files: EmbedUploads(embed),
		Flags: discord.MessageFlagEphemeral, AllowedMentions: noPings(),
	})
}

// RespondPublic answers in the channel for everyone, for a view somebody
// asked to share (/rapsheet view public:true); Discord will not forward an
// ephemeral message.
func (ix *Ix) RespondPublic(embed discord.Embed, components []discord.LayoutComponent) error {
	return ix.respond(discord.InteractionResponseTypeCreateMessage, discord.MessageCreate{
		Embeds: []discord.Embed{embed}, Components: components, Files: EmbedUploads(embed), AllowedMentions: noPings(),
	})
}

// errAlreadyAcknowledged is Discord's 40060: the interaction was already
// responded to or deferred.
const errAlreadyAcknowledged = 40060

// Tell answers privately with msg, falling back to a follow-up when the
// interaction was already acknowledged. The router's refusals and its panic
// recovery use it; the fallback matters most there, since a handler that
// deferred and then panicked would otherwise leave the member with nothing
// and the log with nothing either.
func (ix *Ix) Tell(msg string) {
	err := ix.RespondText(msg)
	if err == nil {
		return
	}
	if re, ok := errors.AsType[*rest.Error](err); ok && re.Code == errAlreadyAcknowledged {
		_, _ = ix.Rest.CreateFollowupMessage(ix.I.ApplicationID(), ix.I.Token(), discord.MessageCreate{
			Content: msg, Flags: discord.MessageFlagEphemeral, AllowedMentions: noPings(),
		})
	}
}

// RespondText answers with plain text, privately.
func (ix *Ix) RespondText(content string) error {
	return ix.respond(discord.InteractionResponseTypeCreateMessage, discord.MessageCreate{
		Content: content, Flags: discord.MessageFlagEphemeral, AllowedMentions: noPings(),
	})
}

// RespondEmbedWithComponents is RespondEmbed with controls under it.
func (ix *Ix) RespondEmbedWithComponents(embed discord.Embed, components []discord.LayoutComponent) error {
	return ix.respond(discord.InteractionResponseTypeCreateMessage, discord.MessageCreate{
		Embeds: []discord.Embed{embed}, Components: components, Files: EmbedUploads(embed),
		Flags: discord.MessageFlagEphemeral, AllowedMentions: noPings(),
	})
}

// RespondOK, RespondErr, RespondInfo and RespondWarn are the core helpers
// of the same names.
func (ix *Ix) RespondOK(title, msg string) { _ = ix.RespondEmbed(Embed(ColorSuccess, title, msg)) }

func (ix *Ix) RespondErr(title string, err error) {
	_ = ix.RespondEmbed(Embed(ColorError, title, err.Error()))
}

func (ix *Ix) RespondInfo(title, msg string) { _ = ix.RespondEmbed(Embed(ColorInfo, title, msg)) }

func (ix *Ix) RespondWarn(title, msg string) { _ = ix.RespondEmbed(Embed(ColorWarning, title, msg)) }

// Defer is DeferResponse: acknowledged privately, answered by a FollowUp.
func (ix *Ix) Defer() error {
	return ix.respond(discord.InteractionResponseTypeDeferredCreateMessage, discord.MessageCreate{Flags: discord.MessageFlagEphemeral})
}

// DeferPublic is DeferResponsePublic, for an answer meant for the channel.
func (ix *Ix) DeferPublic() error {
	return ix.respond(discord.InteractionResponseTypeDeferredCreateMessage, nil)
}

// FollowUpOK, FollowUpErr, FollowUpEmbed and FollowUpEmbedWithFiles are the
// core helpers of the same names: the deferred placeholder replaced.
func (ix *Ix) FollowUpOK(title, msg string) error {
	return ix.FollowUpEmbed(Embed(ColorSuccess, title, msg))
}

func (ix *Ix) FollowUpErr(title string, err error) error {
	return ix.FollowUpEmbed(Embed(ColorError, title, err.Error()))
}

func (ix *Ix) FollowUpEmbed(embed discord.Embed) error {
	return ix.FollowUpEmbedWithFiles(embed)
}

func (ix *Ix) FollowUpEmbedWithFiles(embed discord.Embed, extra ...*discord.File) error {
	return ix.edit(discord.MessageUpdate{
		Embeds: &[]discord.Embed{embed}, Files: append(EmbedUploads(embed), extra...),
		Attachments: noAttachments(), AllowedMentions: noPings(),
	})
}

// FollowUpEmbedWithComponents is the core helper of the same name.
func (ix *Ix) FollowUpEmbedWithComponents(embed discord.Embed, components []discord.LayoutComponent) error {
	return ix.edit(discord.MessageUpdate{
		Embeds: &[]discord.Embed{embed}, Components: &components, Files: EmbedUploads(embed),
		Attachments: noAttachments(), AllowedMentions: noPings(),
	})
}

// DeferUpdate is DeferUpdate: a click acknowledged, nothing changed yet.
func (ix *Ix) DeferUpdate() error {
	return ix.respond(discord.InteractionResponseTypeDeferredUpdateMessage, nil)
}

// UpdateEmbedWithComponents redraws the message a click arrived on.
func (ix *Ix) UpdateEmbedWithComponents(embed discord.Embed, components []discord.LayoutComponent) error {
	return ix.respond(discord.InteractionResponseTypeUpdateMessage, discord.MessageUpdate{
		Embeds: &[]discord.Embed{embed}, Components: &components, Files: EmbedUploads(embed),
		Attachments: noAttachments(), AllowedMentions: noPings(),
	})
}

// RespondPanel, UpdatePanel, DeferPanelUpdate and FollowUpPanel are the
// panel helpers of the same names, for a Panel built from Parts.
func (ix *Ix) RespondPanel(p Panel) error {
	return ix.respond(discord.InteractionResponseTypeCreateMessage, discord.MessageCreate{
		Components: p.Layout(), Files: p.Uploads(),
		Flags: discord.MessageFlagEphemeral | discord.MessageFlagIsComponentsV2, AllowedMentions: noPings(),
	})
}

// RespondPanelPublic posts p in the channel for everyone, for the one
// surface meant to be read by the whole server (/help post).
func (ix *Ix) RespondPanelPublic(p Panel) error {
	return ix.respond(discord.InteractionResponseTypeCreateMessage, discord.MessageCreate{
		Components: p.Layout(), Files: p.Uploads(), Flags: discord.MessageFlagIsComponentsV2, AllowedMentions: noPings(),
	})
}

// OnPrivateMessage is whether a click came from a message only the clicker
// can see. A click on a public one is answered with a private copy rather
// than by redrawing what everyone sees.
func (ix *Ix) OnPrivateMessage() bool {
	c, ok := ix.I.(discord.ComponentInteraction)
	return ok && c.Message.Flags.Has(discord.MessageFlagEphemeral)
}

func (ix *Ix) UpdatePanel(p Panel) error {
	c, flags := p.Layout(), discord.MessageFlagIsComponentsV2
	return ix.respond(discord.InteractionResponseTypeUpdateMessage, discord.MessageUpdate{
		Components: &c, Files: p.Uploads(), Attachments: noAttachments(), Flags: &flags, AllowedMentions: noPings(),
	})
}

func (ix *Ix) DeferPanelUpdate() error { return ix.DeferUpdate() }

func (ix *Ix) FollowUpPanel(p Panel) error {
	c := p.Layout()
	return ix.edit(discord.MessageUpdate{Components: &c, Files: p.Uploads(), Attachments: noAttachments(), AllowedMentions: noPings()})
}

// OpenModal answers with a form; see the core helper of the same name.
func (ix *Ix) OpenModal(customID, title string, fields ...discord.LayoutComponent) error {
	return ix.respond(discord.InteractionResponseTypeModal, discord.ModalCreate{CustomID: customID, Title: title, Components: fields})
}

// Args is LeafArgs: the invoked leaf's options by name. disgo has already
// walked the subcommand path, so this is its map.
func (ix *Ix) Args() map[string]discord.SlashCommandOption {
	if c, ok := ix.I.(discord.ApplicationCommandInteraction); ok {
		if d, ok := c.Data.(discord.SlashCommandInteractionData); ok {
			return d.Options
		}
	}
	return nil
}

// CustomID is a click's or a form's custom ID, "" for a command.
func (ix *Ix) CustomID() string {
	switch i := ix.I.(type) {
	case discord.ComponentInteraction:
		return i.Data.CustomID()
	case discord.ModalSubmitInteraction:
		return i.Data.CustomID
	}
	return ""
}

// Values is what a select submitted, entity picks as their IDs.
func (ix *Ix) Values() []string {
	c, ok := ix.I.(discord.ComponentInteraction)
	if !ok {
		return nil
	}
	var ids []snowflake.ID
	switch d := c.Data.(type) {
	case discord.StringSelectMenuInteractionData:
		return d.Values
	case discord.UserSelectMenuInteractionData:
		ids = d.Values
	case discord.RoleSelectMenuInteractionData:
		ids = d.Values
	case discord.ChannelSelectMenuInteractionData:
		ids = d.Values
	case discord.MentionableSelectMenuInteractionData:
		ids = d.Values
	}
	out := make([]string, len(ids))
	for n, id := range ids {
		out[n] = id.String()
	}
	return out
}

// FirstValue is the first value a select submitted, or "".
func (ix *Ix) FirstValue() string {
	if v := ix.Values(); len(v) > 0 {
		return v[0]
	}
	return ""
}

// ModalValues is ModalValues: a form's text inputs by custom ID.
func (ix *Ix) ModalValues() map[string]string {
	out := map[string]string{}
	m, ok := ix.I.(discord.ModalSubmitInteraction)
	if !ok {
		return out
	}
	for c := range m.Data.AllComponents() {
		if in, ok := c.(discord.TextInputComponent); ok {
			out[in.CustomID] = in.Value
		}
	}
	return out
}

// SelectDiff is SelectDiff: what a member changed on a select drawn with
// the current values preselected, diffed against what the message they
// clicked on really showed.
func (ix *Ix) SelectDiff(customID string) (added, removed []string) {
	var shown []string
	if c, ok := ix.I.(discord.ComponentInteraction); ok {
		for comp := range c.Message.AllComponents() {
			shown = append(shown, preselected(comp, customID)...)
		}
	}
	picked := ix.Values()
	for _, v := range picked {
		if !slices.Contains(shown, v) {
			added = append(added, v)
		}
	}
	for _, v := range shown {
		if !slices.Contains(picked, v) {
			removed = append(removed, v)
		}
	}
	return added, removed
}

// preselected is what comp drew as already picked, if it is the select
// customID.
func preselected(comp discord.Component, customID string) []string {
	var defaults []discord.SelectMenuDefaultValue
	switch m := comp.(type) {
	case discord.StringSelectMenuComponent:
		if m.CustomID != customID {
			return nil
		}
		var out []string
		for _, o := range m.Options {
			if o.Default {
				out = append(out, o.Value)
			}
		}
		return out
	case discord.UserSelectMenuComponent:
		if m.CustomID != customID {
			return nil
		}
		defaults = m.DefaultValues
	case discord.RoleSelectMenuComponent:
		if m.CustomID != customID {
			return nil
		}
		defaults = m.DefaultValues
	case discord.ChannelSelectMenuComponent:
		if m.CustomID != customID {
			return nil
		}
		defaults = m.DefaultValues
	case discord.MentionableSelectMenuComponent:
		if m.CustomID != customID {
			return nil
		}
		defaults = m.DefaultValues
	}
	out := make([]string, 0, len(defaults))
	for _, d := range defaults {
		out = append(out, d.ID.String())
	}
	return out
}
