package roles

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The slash-command handlers are driven end to end against a discordgo
// session whose transport records every REST call and answers it with an
// empty success, the same shape aimod's handler tests use. The assertions
// are about what the handler did (records, role edits, audit rows) plus the
// title of what it said, read back off the recorded request body. No
// network, no Discord.

type recordingTransport struct {
	mu     sync.Mutex
	bodies []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	// JSON escapes < and >, which every mention carries.
	text := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(body))
	r.mu.Lock()
	r.bodies = append(r.bodies, text)
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

// said reports whether any reply the handler sent carried text.
func (r *recordingTransport) said(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bodies {
		if strings.Contains(b, text) {
			return true
		}
	}
	return false
}

func handlerSession(t *testing.T) (*discordgo.Session, *recordingTransport) {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	rt := &recordingTransport{}
	s.Client = &http.Client{Transport: rt}
	return s, rt
}

// rolesInteraction builds one /roles leaf. group is "" for a top-level
// subcommand and "configure" for that group's leaves.
func rolesInteraction(group, sub string, args ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	leaf := &discordgo.ApplicationCommandInteractionDataOption{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: args}
	opts := []*discordgo.ApplicationCommandInteractionDataOption{leaf}
	if group != "" {
		opts = []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: group, Type: discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{leaf},
		}}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i1", Token: "tok", GuildID: "g1", ChannelID: "chan",
		Type:   discordgo.InteractionApplicationCommand,
		Member: &discordgo.Member{User: &discordgo.User{ID: "actor"}},
		Data:   discordgo.ApplicationCommandInteractionData{Name: "roles", Options: opts},
	}}
}

func optOf(name string, typ discordgo.ApplicationCommandOptionType, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: typ, Value: value}
}

func strArg(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return optOf(name, discordgo.ApplicationCommandOptionString, v)
}
func userArg(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return optOf(name, discordgo.ApplicationCommandOptionUser, v)
}
func roleArg(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return optOf(name, discordgo.ApplicationCommandOptionRole, v)
}
func channelArg(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return optOf(name, discordgo.ApplicationCommandOptionChannel, v)
}

// handlerFixture is a plugin with one ordinary member and a guild whose
// jail role already exists, so a jail costs one role edit and no role
// creation unless a test wants otherwise.
func handlerFixture() (*Plugin, *fakeOps, *fakeStore, *fakeAudit, *fakePerms, *fakeSettings) {
	ops := newFakeOps()
	ops.setMember("g1", "u1", []string{"role-a"})
	ops.roles["g1"] = []*discordgo.Role{{ID: "role-a", Name: "a"}, {ID: "role-b", Name: "b"}, {ID: "jail-role", Name: jailRoleName}}
	store, audit, perms, settings := newFakeStore(), newFakeAudit(), newFakePerms(), newFakeSettings()
	p := newTestPlugin(ops, store, settings, audit, perms, newFakeScheduler())
	return p, ops, store, audit, perms, settings
}

func auditActions(a *fakeAudit) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.records))
	for _, r := range a.records {
		out = append(out, r.action)
	}
	return out
}

// TestInitRegistersAFullyWiredCommandTree: every /roles leaf has a handler
// and a declared tier, which Finalize is what enforces. A new leaf that
// misses either fails here rather than on a live server.
func TestInitRegistersAFullyWiredCommandTree(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := core.NewCommandRouter(core.NewPermissions(nil, nil, ""), nil, log)
	p := New(newFakeStore(), newFakeSettings(), func(string) DiscordMemberOps { return newFakeOps() }, func(string) bool { return false }, testVoice(), nil)
	if p.Name() != "roles" {
		t.Fatalf("Name: %q", p.Name())
	}
	if err := p.Init(core.Deps{Commands: router, Logger: log, Audit: newFakeAudit(), Scheduler: newFakeScheduler()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestHandleJailSingleMember: the one-member path jails, audits as
// roles.jail under the actor, DMs the member, and announces in the channel.
func TestHandleJailSingleMember(t *testing.T) {
	p, ops, store, audit, _, _ := handlerFixture()
	s, rt := handlerSession(t)

	p.handleJail(context.Background(), s, rolesInteraction("", "jail", userArg("user", "u1"), strArg("duration", "2h"), strArg("reason", "spam")))

	rec, ok, _ := store.GetJail(context.Background(), "g1", "u1")
	if !ok || rec.JailedBy != "actor" || rec.Reason != "spam" {
		t.Fatalf("expected a jail by the actor, got %+v ok=%v", rec, ok)
	}
	if got := auditActions(audit); len(got) != 1 || got[0] != "roles.jail" {
		t.Fatalf("expected one roles.jail audit entry, got %v", got)
	}
	var dm, announced bool
	for _, m := range ops.dmSends {
		dm = dm || m.channelID == "dm:u1"
		announced = announced || m.channelID == "chan"
	}
	if !dm || !announced {
		t.Fatalf("expected a DM and a channel announcement, dm=%v announced=%v", dm, announced)
	}
	if !rt.said("Member jailed") {
		t.Fatal("expected the success reply")
	}
}

// TestHandleJailRefusesBeforeWriting: bad input, dry-run, an outranking
// target and an unmanageable marker role all leave no record, no role edit
// and no role created.
func TestHandleJailRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []*discordgo.ApplicationCommandInteractionDataOption
		setup func(p *Plugin, perms *fakePerms, settings *fakeSettings)
		reply string
	}{
		{"invalid duration", []*discordgo.ApplicationCommandInteractionDataOption{userArg("user", "u1"), strArg("duration", "soon")}, nil, "Invalid duration"},
		{"no members", []*discordgo.ApplicationCommandInteractionDataOption{userArg("user", ""), strArg("duration", "1h")}, nil, "No members given"},
		{"dry-run", []*discordgo.ApplicationCommandInteractionDataOption{userArg("user", "u1"), strArg("duration", "1h")},
			func(p *Plugin, _ *fakePerms, _ *fakeSettings) { p.dryRun = func(string) bool { return true } }, "Dry-run"},
		{"target outranks actor", []*discordgo.ApplicationCommandInteractionDataOption{userArg("user", "u1"), strArg("duration", "1h")},
			func(_ *Plugin, perms *fakePerms, _ *fakeSettings) { perms.protected["u1"] = true }, "outranks you"},
		{"unmanageable marker role", []*discordgo.ApplicationCommandInteractionDataOption{userArg("user", "u1"), strArg("duration", "1h")},
			func(_ *Plugin, perms *fakePerms, settings *fakeSettings) {
				settings.markerRole["g1"] = "role-b"
				perms.unmanageable["role-b"] = true
			}, "Cannot use configured jail role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ops, store, _, perms, settings := handlerFixture()
			if tc.setup != nil {
				tc.setup(p, perms, settings)
			}
			s, rt := handlerSession(t)

			p.handleJail(context.Background(), s, rolesInteraction("", "jail", tc.args...))

			if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); ok {
				t.Fatal("a refusal must record nothing")
			}
			if len(ops.memberEditCalls["u1"]) != 0 {
				t.Fatal("a refusal must edit nobody")
			}
			if !rt.said(tc.reply) {
				t.Fatalf("expected the reply to say %q", tc.reply)
			}
		})
	}
}

// TestHandleJailResentencesAnAlreadyJailedMember: jailing someone already
// in moves their date, audits as roles.jail_resentenced rather than a second
// roles.jail, and DMs the new end.
func TestHandleJailResentencesAnAlreadyJailedMember(t *testing.T) {
	p, ops, store, audit, _, _ := handlerFixture()
	ops.setMember("g1", "u1", []string{"jail-role"})
	end := fixedNow.Add(time.Minute)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", SnapshotRoleIDs: []string{"role-a"}, ReleaseAt: &end})
	s, rt := handlerSession(t)

	p.handleJail(context.Background(), s, rolesInteraction("", "jail", userArg("user", "u1"), strArg("duration", "3h")))

	rec, _, _ := store.GetJail(context.Background(), "g1", "u1")
	if !rec.ReleaseAt.Equal(fixedNow.Add(3 * time.Hour)) {
		t.Fatalf("expected the sentence moved to 3h, got %v", rec.ReleaseAt)
	}
	if got := auditActions(audit); len(got) != 1 || got[0] != "roles.jail_resentenced" {
		t.Fatalf("expected a roles.jail_resentenced entry, got %v", got)
	}
	if !rt.said("Sentence updated") {
		t.Fatal("expected the re-sentence reply")
	}
}

// TestHandleJailSeveralMembersAuditsOnceAndDMsNobody: the batch path writes
// one roles.jail_bulk entry and sends no DMs, so a raid cleanup cannot spend
// the message budget the releases would need.
func TestHandleJailSeveralMembersAuditsOnceAndDMsNobody(t *testing.T) {
	p, ops, store, audit, _, _ := handlerFixture()
	ops.setMember("g1", "u2", []string{"role-b"})
	s, rt := handlerSession(t)

	p.handleJail(context.Background(), s, rolesInteraction("", "jail", userArg("user", "u1"), strArg("duration", "1h"), userArg("user2", "u2")))

	for _, u := range []string{"u1", "u2"} {
		if _, ok, _ := store.GetJail(context.Background(), "g1", u); !ok {
			t.Fatalf("expected %s jailed", u)
		}
	}
	if got := auditActions(audit); len(got) != 1 || got[0] != "roles.jail_bulk" {
		t.Fatalf("expected one roles.jail_bulk entry, got %v", got)
	}
	for _, m := range ops.dmSends {
		if strings.HasPrefix(m.channelID, "dm:") {
			t.Fatal("a batch jail must DM nobody")
		}
	}
	if !rt.said("Jailed 2 of 2") {
		t.Fatal("expected the batch summary")
	}
}

// TestHandleJailRoleJailsEveryHolderButTheActor: the role path enumerates
// holders, drops the actor, jails the rest and audits once.
func TestHandleJailRoleJailsEveryHolderButTheActor(t *testing.T) {
	p, ops, store, audit, _, _ := handlerFixture()
	ops.setMember("g1", "actor", []string{"raider"})
	ops.setMember("g1", "u2", []string{"raider"})
	ops.setMember("g1", "u3", []string{"raider", "role-a"})
	s, rt := handlerSession(t)

	p.handleJailRole(context.Background(), s, rolesInteraction("", "jail-role", roleArg("role", "raider"), strArg("duration", "1d"), strArg("reason", "raid")))

	for _, u := range []string{"u2", "u3"} {
		if _, ok, _ := store.GetJail(context.Background(), "g1", u); !ok {
			t.Fatalf("expected %s jailed", u)
		}
	}
	for _, u := range []string{"actor", "u1"} {
		if _, ok, _ := store.GetJail(context.Background(), "g1", u); ok {
			t.Fatalf("%s must not be jailed", u)
		}
	}
	if got := auditActions(audit); len(got) != 1 || got[0] != "roles.jail_bulk" {
		t.Fatalf("expected one roles.jail_bulk entry, got %v", got)
	}
	if !rt.said("Jailed 2 member(s) from role") {
		t.Fatal("expected the batch summary")
	}
}

// TestHandleJailRoleRefusals: every path that must jail nobody, from the
// @everyone guard through an over-cap role, a role nobody else holds, an
// unlistable guild, dry-run and a batch the actor outranks nobody in.
func TestHandleJailRoleRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  string
		setup func(p *Plugin, ops *fakeOps, perms *fakePerms)
		reply string
	}{
		{"everyone", "g1", nil, "Cannot jail that role"},
		{"invalid duration", "raider", nil, "Invalid duration"},
		{"only the actor holds it", "raider", func(_ *Plugin, ops *fakeOps, _ *fakePerms) {
			ops.setMember("g1", "actor", []string{"raider"})
		}, "Nobody to jail"},
		{"member list unavailable", "raider", func(_ *Plugin, ops *fakeOps, _ *fakePerms) {
			ops.memberListErr = transientErr()
		}, "Failed to list members"},
		{"over the cap", "raider", func(_ *Plugin, ops *fakeOps, _ *fakePerms) {
			for i := 0; i <= maxBulkJailTargets; i++ {
				ops.setMember("g1", fmt.Sprintf("r%03d", i), []string{"raider"})
			}
		}, "Too many members"},
		{"dry-run", "raider", func(p *Plugin, ops *fakeOps, _ *fakePerms) {
			ops.setMember("g1", "u2", []string{"raider"})
			p.dryRun = func(string) bool { return true }
		}, "Dry-run"},
		{"everyone outranks the actor", "raider", func(_ *Plugin, ops *fakeOps, perms *fakePerms) {
			ops.setMember("g1", "u2", []string{"raider"})
			perms.protected["u2"] = true
		}, "Nobody was jailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ops, store, _, perms, _ := handlerFixture()
			if tc.setup != nil {
				tc.setup(p, ops, perms)
			}
			duration := "1h"
			if tc.name == "invalid duration" {
				duration = "soon"
			}
			s, rt := handlerSession(t)

			p.handleJailRole(context.Background(), s, rolesInteraction("", "jail-role", roleArg("role", tc.role), strArg("duration", duration)))

			store.mu.Lock()
			jailed := len(store.jails)
			store.mu.Unlock()
			if jailed != 0 {
				t.Fatalf("a refusal must jail nobody, got %d records", jailed)
			}
			if !rt.said(tc.reply) {
				t.Fatalf("expected the reply to say %q", tc.reply)
			}
		})
	}
}

// TestHandleRelease: an untracked member is refused without touching them;
// a tracked one is restored, untracked and announced.
func TestHandleRelease(t *testing.T) {
	p, ops, store, _, _, _ := handlerFixture()
	s, rt := handlerSession(t)

	p.handleRelease(context.Background(), s, rolesInteraction("", "release", userArg("user", "u1")))
	if len(ops.memberEditCalls["u1"]) != 0 || !rt.said("Not jailed") {
		t.Fatal("releasing an untracked member must refuse and edit nobody")
	}

	ops.setMember("g1", "u1", []string{"jail-role"})
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", SnapshotRoleIDs: []string{"role-a"}})
	p.handleRelease(context.Background(), s, rolesInteraction("", "release", userArg("user", "u1")))

	if _, ok, _ := store.GetJail(context.Background(), "g1", "u1"); ok {
		t.Fatal("expected the jail untracked")
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 1 || m.Roles[0] != "role-a" {
		t.Fatalf("expected roles restored, got %v", m.Roles)
	}
	var announced bool
	for _, msg := range ops.dmSends {
		announced = announced || msg.channelID == "chan"
	}
	if !announced || !rt.said("Member released") {
		t.Fatal("expected the release announced and confirmed")
	}
}

// TestHandleGrant: a permanent grant adds the role and tracks it with no
// expiry and no timer; a timed one tracks the expiry and arms a timer;
// an unmanageable role or a bad duration adds nothing.
func TestHandleGrant(t *testing.T) {
	p, ops, store, audit, perms, _ := handlerFixture()
	ft := &fakeTimers{}
	p.afterFunc = ft.afterFunc
	s, rt := handlerSession(t)

	perms.unmanageable["role-b"] = true
	p.handleGrant(context.Background(), s, rolesInteraction("", "grant", userArg("user", "u1"), roleArg("role", "role-b")))
	p.handleGrant(context.Background(), s, rolesInteraction("", "grant", userArg("user", "u1"), roleArg("role", "role-c"), strArg("duration", "soon")))
	if len(ops.roleAddCalls) != 0 || !rt.said("Cannot manage that role") || !rt.said("Invalid duration") {
		t.Fatal("a refused grant must add nothing")
	}

	p.handleGrant(context.Background(), s, rolesInteraction("", "grant", userArg("user", "u1"), roleArg("role", "role-c")))
	rec, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "role-c")
	if !ok || rec.ExpiresAt != nil || rec.GrantedBy != "actor" {
		t.Fatalf("expected a permanent grant by the actor, got %+v ok=%v", rec, ok)
	}
	if len(ft.armed) != 0 {
		t.Fatal("a permanent grant arms no timer")
	}

	p.handleGrant(context.Background(), s, rolesInteraction("", "grant", userArg("user", "u1"), roleArg("role", "role-d"), strArg("duration", "90m"), strArg("reason", "helper")))
	rec, ok, _ = store.GetGrant(context.Background(), "g1", "u1", "role-d")
	if !ok || rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(fixedNow.Add(90*time.Minute)) || rec.Reason != "helper" {
		t.Fatalf("expected a timed grant 90m out, got %+v ok=%v", rec, ok)
	}
	if len(ft.armed) != 1 || ft.armed[0].delay != 90*time.Minute {
		t.Fatalf("expected one timer 90m out, got %+v", ft.armed)
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 3 {
		t.Fatalf("expected both roles added, got %v", m.Roles)
	}
	if got := auditActions(audit); len(got) != 2 || got[0] != "roles.grant" {
		t.Fatalf("expected two roles.grant entries, got %v", got)
	}
	if !rt.said("permanently") || !rt.said("for 90m") {
		t.Fatal("expected both grant confirmations")
	}
}

// TestHandleRevoke: a grant merlin is not tracking is refused rather than
// revoked, since a mod can do that in Discord and this bot should not claim
// ownership of roles it never granted; a tracked one is removed and
// untracked.
func TestHandleRevoke(t *testing.T) {
	p, ops, store, _, _, _ := handlerFixture()
	s, rt := handlerSession(t)

	p.handleRevoke(context.Background(), s, rolesInteraction("", "revoke", userArg("user", "u1"), roleArg("role", "role-a")))
	if len(ops.roleRemoveCalls) != 0 || !rt.said("Not a tracked grant") {
		t.Fatal("an untracked grant must be refused without touching the member")
	}

	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-a"})
	p.handleRevoke(context.Background(), s, rolesInteraction("", "revoke", userArg("user", "u1"), roleArg("role", "role-a")))
	if _, ok, _ := store.GetGrant(context.Background(), "g1", "u1", "role-a"); ok {
		t.Fatal("expected the grant untracked")
	}
	m, _ := ops.GuildMember("g1", "u1")
	if len(m.Roles) != 0 || !rt.said("Role revoked") {
		t.Fatalf("expected the role removed and confirmed, got %v", m.Roles)
	}
}

// TestHandleList renders nothing, then a jail plus grants in a stable
// order.
func TestHandleList(t *testing.T) {
	p, _, store, _, _, _ := handlerFixture()
	s, rt := handlerSession(t)

	p.handleList(context.Background(), s, rolesInteraction("", "list", userArg("user", "u1")))
	if !rt.said("No active jail or grants") {
		t.Fatal("expected the empty reply")
	}

	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "g1", UserID: "u1", JailRoleID: "jail-role", ReleaseAt: &end})
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-a"})
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "g1", UserID: "u1", RoleID: "role-b", ExpiresAt: &end})
	p.handleList(context.Background(), s, rolesInteraction("", "list", userArg("user", "u1")))
	for _, want := range []string{"Jailed", "role-a", "permanent", "role-b", "expires"} {
		if !rt.said(want) {
			t.Fatalf("expected the status to mention %q", want)
		}
	}
}

// TestConfigureAllowAndDisallowChannel: the allowlist follows the commands,
// each change is audited, and the live overwrite on that channel follows
// once the jail role is known.
func TestConfigureAllowAndDisallowChannel(t *testing.T) {
	p, ops, _, audit, _, settings := handlerFixture()
	ops.channel["c1"] = &discordgo.Channel{ID: "c1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	p.jailRoleID["g1"] = "jail-role"
	s, rt := handlerSession(t)

	p.handleAllowChannel(context.Background(), s, rolesInteraction("configure", "allow-channel", channelArg("channel", "c1")))
	if got := settings.JailAllowedChannelIDs("g1"); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("expected c1 allowed, got %v", got)
	}
	if ow := ops.overwrites[overwriteKey{"c1", "jail-role"}]; ow.allow&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected c1 made visible to the jail role, got %+v", ow)
	}

	p.handleDisallowChannel(context.Background(), s, rolesInteraction("configure", "disallow-channel", channelArg("channel", "c1")))
	if got := settings.JailAllowedChannelIDs("g1"); len(got) != 0 {
		t.Fatalf("expected c1 removed, got %v", got)
	}
	if ow := ops.overwrites[overwriteKey{"c1", "jail-role"}]; ow.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected c1 hidden from the jail role again, got %+v", ow)
	}
	if got := auditActions(audit); len(got) != 2 {
		t.Fatalf("expected both changes audited, got %v", got)
	}
	if !rt.said("Channel allowed") || !rt.said("Channel hidden") {
		t.Fatal("expected both confirmations")
	}
}

// TestConfigureAllowChannelWithMarkerRoleSyncsEverything: passing a marker
// role alongside the channel switches the guild's jail role and resyncs
// every channel against it, since the cache the one-channel sync reads was
// just cleared.
func TestConfigureAllowChannelWithMarkerRoleSyncsEverything(t *testing.T) {
	p, ops, _, _, _, settings := handlerFixture()
	ops.channel["c1"] = &discordgo.Channel{ID: "c1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	ops.channel["c2"] = &discordgo.Channel{ID: "c2", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	p.jailRoleID["g1"] = "jail-role"
	s, _ := handlerSession(t)

	p.handleAllowChannel(context.Background(), s, rolesInteraction("configure", "allow-channel", channelArg("channel", "c1"), roleArg("marker_role", "role-b")))

	if settings.JailMarkerRoleID("g1") != "role-b" {
		t.Fatal("expected the marker role saved")
	}
	if _, cached := p.jailRoleID["g1"]; cached {
		t.Fatal("expected the old cached jail role forgotten")
	}
	if ow := ops.overwrites[overwriteKey{"c1", "role-b"}]; ow.allow&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected c1 allowed for the new role, got %+v", ow)
	}
	if ow := ops.overwrites[overwriteKey{"c2", "role-b"}]; ow.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected c2 denied for the new role, got %+v", ow)
	}
}

// TestConfigureMarkerRoleSetAndClear: setting a marker role saves it,
// drops the cache and resyncs; omitting the option clears it and drops the
// cache so the fallback role is resolved next time.
func TestConfigureMarkerRoleSetAndClear(t *testing.T) {
	p, ops, _, audit, _, settings := handlerFixture()
	ops.channel["c1"] = &discordgo.Channel{ID: "c1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	s, rt := handlerSession(t)

	p.handleMarkerRole(context.Background(), s, rolesInteraction("configure", "marker-role", roleArg("marker_role", "role-b")))
	if settings.JailMarkerRoleID("g1") != "role-b" || !rt.said("Configured jail role") {
		t.Fatal("expected the marker role saved and confirmed")
	}
	if _, ok := ops.overwrites[overwriteKey{"c1", "role-b"}]; !ok {
		t.Fatal("expected every channel synced against the new role")
	}

	p.jailRoleID["g1"] = "role-b"
	p.handleMarkerRole(context.Background(), s, rolesInteraction("configure", "marker-role"))
	if settings.JailMarkerRoleID("g1") != "" || !rt.said("Cleared jail role") {
		t.Fatal("expected the marker role cleared and confirmed")
	}
	if _, cached := p.jailRoleID["g1"]; cached {
		t.Fatal("clearing must drop the cached role")
	}
	if got := auditActions(audit); len(got) != 2 {
		t.Fatalf("expected both changes audited, got %v", got)
	}
}

// TestConfigureAnnounceChannelSetAndClear mirrors marker-role's
// set-or-omit-to-clear shape for the announcement channel.
func TestConfigureAnnounceChannelSetAndClear(t *testing.T) {
	p, _, _, audit, _, settings := handlerFixture()
	s, rt := handlerSession(t)

	p.handleAnnounceChannel(context.Background(), s, rolesInteraction("configure", "announce-channel", channelArg("channel", "mod-log")))
	if settings.JailAnnounceChannelID("g1") != "mod-log" || !rt.said("Announcement channel set") {
		t.Fatal("expected the announce channel saved and confirmed")
	}
	p.handleAnnounceChannel(context.Background(), s, rolesInteraction("configure", "announce-channel"))
	if settings.JailAnnounceChannelID("g1") != "" || !rt.said("Announcement channel cleared") {
		t.Fatal("expected the announce channel cleared and confirmed")
	}
	if got := auditActions(audit); len(got) != 2 {
		t.Fatalf("expected both changes audited, got %v", got)
	}
}

// TestConfigureListChannels renders the empty case and the configured one,
// including where announcements go.
func TestConfigureListChannels(t *testing.T) {
	p, _, _, _, _, settings := handlerFixture()
	s, rt := handlerSession(t)

	p.handleListChannels(context.Background(), s, rolesInteraction("configure", "list-channels"))
	if !rt.said("No allowed channels") || !rt.said("the invoking channel only") {
		t.Fatal("expected the empty reply")
	}

	settings.allowed["g1"] = []string{"c1", "c2"}
	settings.announce["g1"] = "mod-log"
	p.handleListChannels(context.Background(), s, rolesInteraction("configure", "list-channels"))
	for _, want := range []string{"Channels visible while jailed", "<#c1>", "<#c2>", "<#mod-log>"} {
		if !rt.said(want) {
			t.Fatalf("expected the list to mention %q in %q", want, rt.bodies)
		}
	}
}

// TestConfigureSyncChannels resolves the jail role (creating it if the
// guild has none) and writes every channel's overwrite, reporting a
// fan-out failure as such rather than as success.
func TestConfigureSyncChannels(t *testing.T) {
	p, ops, _, _, _, _ := handlerFixture()
	ops.roles["g1"] = nil // no jail role yet: sync must create it
	ops.channel["c1"] = &discordgo.Channel{ID: "c1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	s, rt := handlerSession(t)

	p.handleSyncChannels(context.Background(), s, rolesInteraction("configure", "sync-channels"))
	if !rt.said("Channels synced") {
		t.Fatal("expected the success reply")
	}
	if _, ok := ops.overwrites[overwriteKey{"c1", "role-created-1"}]; !ok {
		t.Fatalf("expected c1 synced against the created jail role, got %v", ops.overwrites)
	}

	ops.guildChannelsErr = transientErr()
	p.handleSyncChannels(context.Background(), s, rolesInteraction("configure", "sync-channels"))
	if !rt.said("Sync completed with errors") {
		t.Fatal("expected the failure reported")
	}
}
