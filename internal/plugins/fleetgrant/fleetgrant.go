// Package fleetgrant lets another bird on cygnet's link ask merlin to give
// a member one role: skua handing out /pp's beta role when a member redeems
// a key is the first. It is the one way another bot can make merlin write
// to Discord, so it is narrow on purpose:
//
//   - cygnet authenticates, merlin authorizes. The asking bird is the From
//     cygnet stamps on the call from the bird's token, never anything the
//     body says.
//   - A role is granted only if this server's admins allowed exactly that
//     bird to ask for exactly that role (/fleet allow), or, where cygnet
//     is a member of the server (cygnet.here), if cygnet attested the
//     asking bird: she saw it run the very image its CI signed. Either way
//     only while the
//     role carries no Discord permission, isn't managed, sits under
//     merlin's own top role, and is named nowhere in merlin's settings (a
//     mod, member, jail or vacation role, an allow or deny list). All of
//     that is checked at every call, not only when it was allowed.
//   - The member must be a person in the server. merlin only ever adds a
//     role: there is no verb that takes one away or touches anything else.
//   - The body is three snowflakes, decoded strictly. There is no text in
//     it that merlin reads as anything but an ID, and nothing to execute.
//   - Calls are capped per bird per server, go through discordguard (so a
//     pause or dry run holds them), and each grant is audited.
package fleetgrant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/cygnet/link"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// Verb is what another bird calls: merlin.grant.
const Verb = "grant"

// actionManage is allowing, refusing and listing what birds may ask for.
const actionManage = "fleet.manage"

// perMinute caps one bird's grants in one server a minute. A key redeemed
// is one call; this is far above any real use and far below a flood.
const perMinute = 30

// Request is a grant's body: who gets which role where, each a snowflake.
type Request struct {
	Guild string `json:"guild"`
	User  string `json:"user"`
	Role  string `json:"role"`
}

var (
	snowflakeID = regexp.MustCompile(`^[0-9]{17,20}$`)
	birdName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

var (
	errInvalid    = errors.New("fleetgrant: not three snowflakes")
	errNotAllowed = errors.New("fleetgrant: that role isn't allowed for this bird here")
	errNotPerson  = errors.New("fleetgrant: not a person in this server")
	errTooMany    = errors.New("fleetgrant: too many grants this minute")
	errHeld       = errors.New("fleetgrant: writes are paused or in dry run here")
)

// Entry is one role a bird may ask for in a server.
type Entry struct {
	Caller, RoleID, AddedBy string
	AddedAt                 time.Time
}

// Store keeps what each bird may ask for, by server.
type Store interface {
	Allow(ctx context.Context, guildID, caller, roleID, by string) error
	// Deny reports whether there was anything to take away.
	Deny(ctx context.Context, guildID, caller, roleID string) (bool, error)
	Allowed(ctx context.Context, guildID, caller, roleID string) (bool, error)
	List(ctx context.Context, guildID string) ([]Entry, error)
}

// Roles is the part of core.Permissions a grant answers to.
type Roles interface {
	BareRole(guildID, roleID string) error
}

// RoleUse is whether merlin's settings give a role meaning
// (settings.Store.RoleInUse).
type RoleUse interface {
	RoleInUse(guildID, roleID string) bool
}

// MemberOps is the Discord a grant needs: a live look at the member, and
// adding the role. discordguard's GuildOps satisfies it.
type MemberOps interface {
	GetMember(guildID, userID snowflake.ID) (*discord.Member, error)
	AddMemberRole(guildID, userID, roleID snowflake.ID, opts ...rest.RequestOpt) error
}

// Plugin is /fleet and the grant verb.
type Plugin struct {
	store    Store
	use      RoleUse
	ops      func(guildID string) MemberOps
	roles    Roles
	audit    core.AuditWriter
	log      *slog.Logger
	commands *core.CommandRouter
	now      func() time.Time

	mu     sync.Mutex
	minute int64
	calls  map[string]int // by bird and server, this minute

	// here asks cygnet whether she is in a server (Vouch); nil without
	// the link, and then only /fleet allow grants.
	here func(ctx context.Context, guildID string) (bool, error)
}

// Vouch has p ask here whether cygnet keeps watch over a server, and grant
// an attested bird's ask there without its admins listing the role.
func (p *Plugin) Vouch(here func(ctx context.Context, guildID string) (bool, error)) { p.here = here }

// New is the plugin over store, merlin's settings and guarded ops.
func New(store Store, use RoleUse, ops func(guildID string) MemberOps) *Plugin {
	return &Plugin{store: store, use: use, ops: ops, now: time.Now, calls: map[string]int{}}
}

func (p *Plugin) Name() string { return "fleet" }

func (p *Plugin) Init(deps core.Deps) error {
	p.roles, p.audit, p.log, p.commands = deps.Perms, deps.Audit, deps.Logger, deps.Commands
	caller := discord.ApplicationCommandOptionString{Name: "bird", Description: "The bird on cygnet's link, by its name there: skua", Required: true}
	role := discord.ApplicationCommandOptionRole{Name: "role", Description: "A role with no permissions that merlin's settings don't use", Required: true}
	p.commands.RegisterCommand(p.Name(), discord.SlashCommandCreate{
		Name:        "fleet",
		Description: "Which roles other birds may ask merlin to give a member",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{Name: "allow", Description: "Let a bird ask merlin to give members one role", Options: []discord.ApplicationCommandOption{caller, role}},
			discord.ApplicationCommandOptionSubCommand{Name: "deny", Description: "Stop a bird asking for a role", Options: []discord.ApplicationCommandOption{caller, role}},
			discord.ApplicationCommandOptionSubCommand{Name: "list", Description: "Every role a bird may ask for here"},
		},
	})
	spec := core.PermSpec{Tier: core.TierAdmin, Action: actionManage}
	p.commands.HandleIx("fleet", "allow", spec, p.handleAllow)
	p.commands.HandleIx("fleet", "deny", spec, p.handleDeny)
	p.commands.HandleIx("fleet", "list", spec, p.handleList)
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Fleet",
		Line:  "roles other birds may ask me to give",
		About: "other birds on cygnet's link can ask me to give a member a role, like skua handing out a beta role when someone redeems a key. " +
			"i do it for a role an admin allowed with `/fleet allow`, for that bird alone, or, in a server cygnet is in, for a bird she has seen run the build its own repository signed. " +
			"either way only while the role has no permissions and none of my settings use it. " +
			"i never take a role away for them. `/fleet list` shows what's allowed, `/fleet deny` takes it back.",
	}
}

// Answer is the link handler: it decodes the body strictly, grants, and
// says why not in the link's own codes, nothing more.
func (p *Plugin) Answer(ctx context.Context, c link.Call) (any, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(c.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		return nil, link.Err(link.Invalid, "body")
	}
	err := p.GrantAs(ctx, c.From, c.Trust, req)
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, errInvalid):
		return nil, link.Err(link.Invalid, "body")
	case errors.Is(err, errNotAllowed):
		return nil, link.Err(link.Denied, "not allowed")
	case errors.Is(err, errNotPerson):
		return nil, link.Err(link.Denied, "not a member")
	case errors.Is(err, errTooMany), errors.Is(err, errHeld):
		return nil, link.Err(link.Busy, "later")
	}
	p.log.Error("fleet: a grant failed", "caller", c.From, "guild", req.Guild, "err", err)
	return nil, err
}

// Grant gives req's member req's role for caller, if an admin allowed
// caller that role.
func (p *Plugin) Grant(ctx context.Context, caller string, req Request) error {
	return p.GrantAs(ctx, caller, link.Untrusted, req)
}

// GrantAs is Grant for a caller cygnet trusts as far as trust: an attested
// caller needs no admin's listing where cygnet is a member of the server.
func (p *Plugin) GrantAs(ctx context.Context, caller string, trust link.Trust, req Request) error {
	if !birdName.MatchString(caller) || !snowflakeID.MatchString(req.Guild) || !snowflakeID.MatchString(req.User) || !snowflakeID.MatchString(req.Role) {
		return errInvalid
	}
	gid, uid, rid := snowflake.MustParse(req.Guild), snowflake.MustParse(req.User), snowflake.MustParse(req.Role)
	if !p.take(caller + "/" + req.Guild) {
		return errTooMany
	}
	ok, err := p.store.Allowed(ctx, req.Guild, caller, req.Role)
	if err != nil {
		return err
	}
	why := "listed by an admin"
	if !ok && trust == link.Attested && p.here != nil {
		if here, err := p.here(ctx, req.Guild); err != nil {
			p.log.Warn("fleet: asking cygnet whether she's here", "guild", req.Guild, "err", err)
		} else if here {
			ok, why = true, "attested by cygnet, who is here"
		}
	}
	if !ok || p.use.RoleInUse(req.Guild, req.Role) {
		return errNotAllowed
	}
	if err := p.roles.BareRole(req.Guild, req.Role); err != nil {
		return fmt.Errorf("%w: %w", errNotAllowed, err)
	}
	ops := p.ops(req.Guild)
	m, err := ops.GetMember(gid, uid)
	if err != nil || m == nil || m.User.Bot || m.User.System {
		return errNotPerson
	}
	if slices.Contains(m.RoleIDs, rid) {
		return nil
	}
	err = ops.AddMemberRole(gid, uid, rid, rest.WithReason("asked by "+caller+" over cygnet"))
	if discordguard.Skipped(err) {
		return errHeld
	}
	if err != nil {
		return err
	}
	if err := p.audit.Record(ctx, req.Guild, "fleet:"+caller, "fleet.grant", "", fmt.Sprintf("user=%s role=%s bird=%s why=%q", core.MentionUser(req.User), core.MentionRole(req.Role), caller, why)); err != nil {
		p.log.Error("fleet: audit grant failed", "guild", req.Guild, "err", err)
	}
	return nil
}

// take counts one call for key this minute and reports whether it's
// within perMinute.
func (p *Plugin) take(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.now().Unix() / 60; m != p.minute {
		p.minute, p.calls = m, map[string]int{}
	}
	p.calls[key]++
	return p.calls[key] <= perMinute
}

func (p *Plugin) handleAllow(ctx context.Context, ix *core.Ix) {
	caller, role := strings.TrimSpace(ix.Args()["bird"].String()), ix.Args()["role"].String()
	if !birdName.MatchString(caller) {
		ix.RespondErr("Not a bird's name", errors.New("use the name the bird has on cygnet's link, like skua"))
		return
	}
	if p.use.RoleInUse(ix.GuildID, role) {
		ix.RespondErr("That role is in use", errors.New("my settings use it already (a mod, member, jail, vacation or join role, or a permission list), so no other bird may hand it out"))
		return
	}
	if err := p.roles.BareRole(ix.GuildID, role); err != nil {
		ix.RespondErr("Not a role I'll hand out for others", err)
		return
	}
	if err := p.store.Allow(ctx, ix.GuildID, caller, role, ix.UserID); err != nil {
		ix.RespondErr("Failed to save", err)
		return
	}
	if err := p.audit.Record(ctx, ix.GuildID, ix.UserID, "fleet.allow", "", fmt.Sprintf("bird=%s role=%s", caller, core.MentionRole(role))); err != nil {
		p.log.Error("fleet: audit allow failed", "guild", ix.GuildID, "err", err)
	}
	ix.RespondOK("Allowed", fmt.Sprintf("%s may ask me to give members <@&%s> now. i'll refuse if the role ever gains a permission.", caller, role))
}

func (p *Plugin) handleDeny(ctx context.Context, ix *core.Ix) {
	caller, role := strings.TrimSpace(ix.Args()["bird"].String()), ix.Args()["role"].String()
	had, err := p.store.Deny(ctx, ix.GuildID, caller, role)
	if err != nil {
		ix.RespondErr("Failed to save", err)
		return
	}
	if !had {
		ix.RespondInfo("Nothing to take back", fmt.Sprintf("%s couldn't ask for <@&%s> here anyway.", caller, role))
		return
	}
	if err := p.audit.Record(ctx, ix.GuildID, ix.UserID, "fleet.deny", fmt.Sprintf("bird=%s role=%s", caller, core.MentionRole(role)), ""); err != nil {
		p.log.Error("fleet: audit deny failed", "guild", ix.GuildID, "err", err)
	}
	ix.RespondOK("Denied", fmt.Sprintf("%s can't ask me for <@&%s> any more. members who already have it keep it.", caller, role))
}

func (p *Plugin) handleList(ctx context.Context, ix *core.Ix) {
	entries, err := p.store.List(ctx, ix.GuildID)
	if err != nil {
		ix.RespondErr("Failed to look it up", err)
		return
	}
	if len(entries) == 0 {
		ix.RespondInfo("Fleet", "no bird may ask me for any role here.")
		return
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = fmt.Sprintf("%s may ask for <@&%s> (allowed by <@%s>)", e.Caller, e.RoleID, e.AddedBy)
	}
	ix.RespondInfo("Fleet", strings.Join(lines, "\n"))
}
