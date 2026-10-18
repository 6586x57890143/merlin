package roles

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

// The slash-command handlers are driven end to end against a REST client
// whose transport records every REST call and answers it with an
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

// deferredFirst reports whether the first thing the handler sent was a
// deferred acknowledgement (interaction response type 5). The router
// cancels an unanswered handler's context 2.5s in, so a handler doing
// several Discord calls must have said this before any of them.
func deferredFirst(r *recordingTransport) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies) > 0 && strings.HasPrefix(r.bodies[0], `{"type":5`)
}

// current is the recorder the next interaction answers through.
// handlerSession makes a fresh one per case, as it used to make a session.
var current *recordingTransport

func handlerSession(t *testing.T) *recordingTransport {
	t.Helper()
	current = &recordingTransport{}
	return current
}

// decodeIx is Discord's JSON for an interaction of typ decoded by
// core.NewIx, answered through current.
//
// The fixtures predate disgo: the store and fakes key on "7000001", "7000601" and
// "7000101", none of them snowflakes. Ix.GuildID, UserID and ChannelID are
// merlin's own string keys and every handler reads those, so after decoding
// a real interaction they are pointed at the fixtures.
func decodeIx(typ int, guild, actor string, data map[string]any, message map[string]any) *core.Ix {
	p := map[string]any{
		"id": "1000", "application_id": "2", "type": typ, "token": "tok", "version": 1,
		"guild_id": paneltest.IxGuild,
		"channel":  map[string]any{"id": "101", "type": 0},
		"member": map[string]any{
			"user":        map[string]any{"id": "11", "username": "7000101"},
			"permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z",
		},
		"data": data,
	}
	if message != nil {
		p["message"] = message
	}
	raw, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	rt := current
	if rt == nil {
		rt = &recordingTransport{}
	}
	ix, err := core.NewIx(raw, rest.New(rest.NewClient("test", rest.WithHTTPClient(&http.Client{Transport: rt}))))
	if err != nil {
		panic(err)
	}
	ix.GuildID, ix.UserID, ix.ChannelID = guild, actor, "7000601"
	return ix
}

// rolesInteraction builds one /roles leaf. group is "" for a top-level
// subcommand and "configure" for that group's leaves.
func rolesInteraction(group, sub string, args ...map[string]any) *core.Ix {
	opts := []any{}
	for _, a := range args {
		opts = append(opts, a)
	}
	top := []any{map[string]any{"name": sub, "type": 1, "options": opts}}
	if group != "" {
		top = []any{map[string]any{"name": group, "type": 2, "options": top}}
	}
	return decodeIx(2, "7000001", "7000101", map[string]any{"id": "6", "name": "roles", "type": 1, "options": top}, nil)
}

func optOf(name string, typ discord.ApplicationCommandOptionType, value string) map[string]any {
	return map[string]any{"name": name, "type": int(typ), "value": value}
}

func strArg(name, v string) map[string]any {
	return optOf(name, discord.ApplicationCommandOptionTypeString, v)
}
func userArg(name, v string) map[string]any {
	return optOf(name, discord.ApplicationCommandOptionTypeUser, v)
}
func roleArg(name, v string) map[string]any {
	return optOf(name, discord.ApplicationCommandOptionTypeRole, v)
}

// setOn clicks a /roles settings control as actor: picked is what the
// select submits and drawn what it showed preselected, which is what
// Ix.SelectDiff compares. The drawn select is a string select because these
// fixtures' IDs are not snowflakes; SelectDiff reads only values either way.
func setOn(p *Plugin, guild, actor, customID string, drawn []string, picked ...string) {
	var opts []discord.StringSelectMenuOption
	for _, d := range drawn {
		opts = append(opts, discord.StringSelectMenuOption{Label: d, Value: d, Default: true})
	}
	pn := core.Panel{Parts: []discord.ContainerSubComponent{ui.Row(discord.StringSelectMenuComponent{CustomID: customID, Options: opts})}}
	values := []any{}
	for _, v := range picked {
		values = append(values, v)
	}
	i := decodeIx(3, guild, actor, map[string]any{"custom_id": customID, "component_type": 3, "values": values},
		map[string]any{"id": "102", "channel_id": "101", "content": "", "flags": 64,
			"author": map[string]any{"id": "2", "username": "merlin"}, "components": pn.Layout()})
	p.handlePanelSet(context.Background(), i, customID)
}

func set(p *Plugin, customID string, drawn []string, picked ...string) {
	setOn(p, "7000001", "7000101", customID, drawn, picked...)
}

// showPage draws one page of /roles settings in g1.
func showPage(p *Plugin, page string) { showPageIn(p, "7000001", page) }

func showPageIn(p *Plugin, guild, page string) {
	i := decodeIx(3, guild, "7000101", map[string]any{"custom_id": panelView + "page", "component_type": 3, "values": []any{page}}, nil)
	p.handlePanelView(context.Background(), i, panelView+"page")
}

// handlerFixture is a plugin with one ordinary member and a guild whose
// jail role already exists, so a jail costs one role edit and no role
// creation unless a test wants otherwise.
func handlerFixture() (*Plugin, *fakeOps, *fakeStore, *fakeAudit, *fakePerms, *fakeSettings) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	ops.roles["7000001"] = []*frole{{ID: "7000302", Name: "a"}, {ID: "7000303", Name: "b"}, {ID: "7000301", Name: jailRoleName}}
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
	p := New(newFakeStore(), newFakeSettings(), newFakeScripts(), func(string) DiscordMemberOps { return newFakeOps() }, func(string) bool { return false }, testVoice(), nil)
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
	rt := handlerSession(t)

	p.handleJail(context.Background(), rolesInteraction("", "jail", userArg("user", "7000102"), strArg("duration", "2h"), strArg("reason", "spam")))

	rec, ok, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if !ok || rec.JailedBy != "7000101" || rec.Reason != "spam" {
		t.Fatalf("expected a jail by the actor, got %+v ok=%v", rec, ok)
	}
	if got := auditActions(audit); len(got) != 1 || got[0] != "roles.jail" {
		t.Fatalf("expected one roles.jail audit entry, got %v", got)
	}
	var dm, announced bool
	for _, m := range ops.dmSends {
		dm = dm || m.channelID == "97000102"
		announced = announced || m.channelID == "7000601"
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
		args  []map[string]any
		setup func(p *Plugin, perms *fakePerms, settings *fakeSettings)
		reply string
	}{
		{"invalid duration", []map[string]any{userArg("user", "7000102"), strArg("duration", "soon")}, nil, "Invalid duration"},
		{"no members", []map[string]any{userArg("user", ""), strArg("duration", "1h")}, nil, "No members given"},
		{"dry-run", []map[string]any{userArg("user", "7000102"), strArg("duration", "1h")},
			func(p *Plugin, _ *fakePerms, _ *fakeSettings) { p.dryRun = func(string) bool { return true } }, "Dry-run"},
		{"target outranks actor", []map[string]any{userArg("user", "7000102"), strArg("duration", "1h")},
			func(_ *Plugin, perms *fakePerms, _ *fakeSettings) { perms.protected["7000102"] = true }, "outranks you"},
		{"unmanageable marker role", []map[string]any{userArg("user", "7000102"), strArg("duration", "1h")},
			func(_ *Plugin, perms *fakePerms, settings *fakeSettings) {
				settings.markerRole["7000001"] = "7000303"
				perms.unmanageable["7000303"] = true
			}, "Cannot use configured jail role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ops, store, _, perms, settings := handlerFixture()
			if tc.setup != nil {
				tc.setup(p, perms, settings)
			}
			rt := handlerSession(t)

			p.handleJail(context.Background(), rolesInteraction("", "jail", tc.args...))

			if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok {
				t.Fatal("a refusal must record nothing")
			}
			if len(ops.memberEditCalls["7000102"]) != 0 {
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
	ops.setMember("7000001", "7000102", []string{"7000301"})
	end := fixedNow.Add(time.Minute)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})
	rt := handlerSession(t)

	p.handleJail(context.Background(), rolesInteraction("", "jail", userArg("user", "7000102"), strArg("duration", "3h")))

	rec, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
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
	ops.setMember("7000001", "7000103", []string{"7000303"})
	rt := handlerSession(t)

	p.handleJail(context.Background(), rolesInteraction("", "jail", userArg("user", "7000102"), strArg("duration", "1h"), userArg("user2", "7000103")))

	for _, u := range []string{"7000102", "7000103"} {
		if _, ok, _ := store.GetJail(context.Background(), "7000001", u); !ok {
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
	ops.setMember("7000001", "7000101", []string{"7000315"})
	ops.setMember("7000001", "7000103", []string{"7000315"})
	ops.setMember("7000001", "7000104", []string{"7000315", "7000302"})
	rt := handlerSession(t)

	p.handleJailRole(context.Background(), rolesInteraction("", "jail-role", roleArg("role", "7000315"), strArg("duration", "1d"), strArg("reason", "raid")))

	for _, u := range []string{"7000103", "7000104"} {
		if _, ok, _ := store.GetJail(context.Background(), "7000001", u); !ok {
			t.Fatalf("expected %s jailed", u)
		}
	}
	for _, u := range []string{"7000101", "7000102"} {
		if _, ok, _ := store.GetJail(context.Background(), "7000001", u); ok {
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
		{"everyone", "7000001", nil, "Cannot jail that role"},
		{"invalid duration", "7000315", nil, "Invalid duration"},
		{"only the actor holds it", "7000315", func(_ *Plugin, ops *fakeOps, _ *fakePerms) {
			ops.setMember("7000001", "7000101", []string{"7000315"})
		}, "Nobody to jail"},
		{"member list unavailable", "7000315", func(_ *Plugin, ops *fakeOps, _ *fakePerms) {
			ops.memberListErr = transientErr()
		}, "Failed to list members"},
		{"over the cap", "7000315", func(_ *Plugin, ops *fakeOps, _ *fakePerms) {
			for i := 0; i <= maxBulkJailTargets; i++ {
				ops.setMember("7000001", strconv.Itoa(7200000+i), []string{"7000315"})
			}
		}, "Too many members"},
		{"dry-run", "7000315", func(p *Plugin, ops *fakeOps, _ *fakePerms) {
			ops.setMember("7000001", "7000103", []string{"7000315"})
			p.dryRun = func(string) bool { return true }
		}, "Dry-run"},
		{"everyone outranks the actor", "7000315", func(_ *Plugin, ops *fakeOps, perms *fakePerms) {
			ops.setMember("7000001", "7000103", []string{"7000315"})
			perms.protected["7000103"] = true
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
			rt := handlerSession(t)

			p.handleJailRole(context.Background(), rolesInteraction("", "jail-role", roleArg("role", tc.role), strArg("duration", duration)))

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
	rt := handlerSession(t)

	p.handleRelease(context.Background(), rolesInteraction("", "release", userArg("user", "7000102")))
	if len(ops.memberEditCalls["7000102"]) != 0 || !rt.said("Not jailed") {
		t.Fatal("releasing an untracked member must refuse and edit nobody")
	}

	ops.setMember("7000001", "7000102", []string{"7000301"})
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302"}})
	rt = handlerSession(t)
	p.handleRelease(context.Background(), rolesInteraction("", "release", userArg("user", "7000102")))

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected the jail untracked")
	}
	if !deferredFirst(rt) {
		t.Fatalf("a release must defer before its Discord calls, got %v", rt.bodies)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 1 || m.Roles[0] != "7000302" {
		t.Fatalf("expected roles restored, got %v", m.Roles)
	}
	var announced bool
	for _, msg := range ops.dmSends {
		announced = announced || msg.channelID == "7000601"
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
	rt := handlerSession(t)

	perms.unmanageable["7000303"] = true
	p.handleGrant(context.Background(), rolesInteraction("", "grant", userArg("user", "7000102"), roleArg("role", "7000303")))
	p.handleGrant(context.Background(), rolesInteraction("", "grant", userArg("user", "7000102"), roleArg("role", "7000304"), strArg("duration", "soon")))
	if len(ops.roleAddCalls) != 0 || !rt.said("Cannot manage that role") || !rt.said("Invalid duration") {
		t.Fatal("a refused grant must add nothing")
	}

	p.handleGrant(context.Background(), rolesInteraction("", "grant", userArg("user", "7000102"), roleArg("role", "7000304")))
	rec, ok, _ := store.GetGrant(context.Background(), "7000001", "7000102", "7000304")
	if !ok || rec.ExpiresAt != nil || rec.GrantedBy != "7000101" {
		t.Fatalf("expected a permanent grant by the actor, got %+v ok=%v", rec, ok)
	}
	if len(ft.armed) != 0 {
		t.Fatal("a permanent grant arms no timer")
	}

	p.handleGrant(context.Background(), rolesInteraction("", "grant", userArg("user", "7000102"), roleArg("role", "7000305"), strArg("duration", "90m"), strArg("reason", "helper")))
	rec, ok, _ = store.GetGrant(context.Background(), "7000001", "7000102", "7000305")
	if !ok || rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(fixedNow.Add(90*time.Minute)) || rec.Reason != "helper" {
		t.Fatalf("expected a timed grant 90m out, got %+v ok=%v", rec, ok)
	}
	if len(ft.armed) != 1 || ft.armed[0].delay != 90*time.Minute {
		t.Fatalf("expected one timer 90m out, got %+v", ft.armed)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
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
	rt := handlerSession(t)

	p.handleRevoke(context.Background(), rolesInteraction("", "revoke", userArg("user", "7000102"), roleArg("role", "7000302")))
	if len(ops.roleRemoveCalls) != 0 || !rt.said("Not a tracked grant") {
		t.Fatal("an untracked grant must be refused without touching the member")
	}

	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "7000001", UserID: "7000102", RoleID: "7000302"})
	rt = handlerSession(t)
	p.handleRevoke(context.Background(), rolesInteraction("", "revoke", userArg("user", "7000102"), roleArg("role", "7000302")))
	if _, ok, _ := store.GetGrant(context.Background(), "7000001", "7000102", "7000302"); ok {
		t.Fatal("expected the grant untracked")
	}
	if !deferredFirst(rt) {
		t.Fatalf("a revoke must defer before its Discord calls, got %v", rt.bodies)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if len(m.Roles) != 0 || !rt.said("Role revoked") {
		t.Fatalf("expected the role removed and confirmed, got %v", m.Roles)
	}
}

// TestHandleList renders nothing, then a jail plus grants in a stable
// order.
func TestHandleList(t *testing.T) {
	p, _, store, _, _, _ := handlerFixture()
	rt := handlerSession(t)

	p.handleList(context.Background(), rolesInteraction("", "list", userArg("user", "7000102")))
	if !rt.said("No active jail or grants") {
		t.Fatal("expected the empty reply")
	}

	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", ReleaseAt: &end})
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "7000001", UserID: "7000102", RoleID: "7000302"})
	_ = store.InsertGrant(context.Background(), GrantRecord{GuildID: "7000001", UserID: "7000102", RoleID: "7000303", ExpiresAt: &end})
	p.handleList(context.Background(), rolesInteraction("", "list", userArg("user", "7000102")))
	for _, want := range []string{"Jailed", "7000302", "permanent", "7000303", "expires"} {
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
	ops.channel["7000603"] = &fch{ID: "7000603", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	p.jailRoleID["7000001"] = "7000301"
	rt := handlerSession(t)

	set(p, panelJail+"allow", nil, "7000603")
	if got := settings.JailAllowedChannelIDs("7000001"); len(got) != 1 || got[0] != "7000603" {
		t.Fatalf("expected c1 allowed, got %v", got)
	}
	if ow := ops.overwrites[overwriteKey{"7000603", "7000301"}]; ow.allow&discord.PermissionViewChannel == 0 {
		t.Fatalf("expected c1 made visible to the jail role, got %+v", ow)
	}

	set(p, panelJail+"allow", []string{"7000603"})
	if got := settings.JailAllowedChannelIDs("7000001"); len(got) != 0 {
		t.Fatalf("expected c1 removed, got %v", got)
	}
	if ow := ops.overwrites[overwriteKey{"7000603", "7000301"}]; ow.deny&discord.PermissionViewChannel == 0 {
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
	ops.channel["7000603"] = &fch{ID: "7000603", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.channel["7000604"] = &fch{ID: "7000604", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	p.jailRoleID["7000001"] = "7000301"
	handlerSession(t)

	set(p, panelJail+"marker", nil, "7000303")
	set(p, panelJail+"allow", nil, "7000603")

	if settings.JailMarkerRoleID("7000001") != "7000303" {
		t.Fatal("expected the marker role saved")
	}
	if _, cached := p.jailRoleID["7000001"]; cached {
		t.Fatal("expected the old cached jail role forgotten")
	}
	if ow := ops.overwrites[overwriteKey{"7000603", "7000303"}]; ow.allow&discord.PermissionViewChannel == 0 {
		t.Fatalf("expected c1 allowed for the new role, got %+v", ow)
	}
	if ow := ops.overwrites[overwriteKey{"7000604", "7000303"}]; ow.deny&discord.PermissionViewChannel == 0 {
		t.Fatalf("expected c2 denied for the new role, got %+v", ow)
	}
}

// TestConfigureMarkerRoleSetAndClear: setting a marker role saves it,
// drops the cache and resyncs; omitting the option clears it and drops the
// cache so the fallback role is resolved next time.
func TestConfigureMarkerRoleSetAndClear(t *testing.T) {
	p, ops, _, audit, _, settings := handlerFixture()
	ops.channel["7000603"] = &fch{ID: "7000603", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	rt := handlerSession(t)

	set(p, panelJail+"marker", nil, "7000303")
	if settings.JailMarkerRoleID("7000001") != "7000303" || !rt.said("Configured jail role") {
		t.Fatal("expected the marker role saved and confirmed")
	}
	if _, ok := ops.overwrites[overwriteKey{"7000603", "7000303"}]; !ok {
		t.Fatal("expected every channel synced against the new role")
	}

	p.jailRoleID["7000001"] = "7000303"
	set(p, panelJail+"marker", nil)
	if settings.JailMarkerRoleID("7000001") != "" || !rt.said("Cleared jail role") {
		t.Fatal("expected the marker role cleared and confirmed")
	}
	if _, cached := p.jailRoleID["7000001"]; cached {
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
	rt := handlerSession(t)

	set(p, panelJail+"announce", nil, "7000627")
	if settings.JailAnnounceChannelID("7000001") != "7000627" || !rt.said("Announcement channel set") {
		t.Fatal("expected the announce channel saved and confirmed")
	}
	set(p, panelJail+"announce", nil)
	if settings.JailAnnounceChannelID("7000001") != "" || !rt.said("Announcement channel cleared") {
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
	rt := handlerSession(t)

	showPage(p, "jail")
	if !rt.said("Now: nothing") || !rt.said("only where the command was run") {
		t.Fatal("expected the empty reply")
	}

	settings.allowed["7000001"] = []string{"7000603", "7000604"}
	settings.announce["7000001"] = "7000627"
	showPage(p, "jail")
	for _, want := range []string{"Visible while jailed", "<#7000603> <#7000604>", "also <#7000627>"} {
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
	ops.roles["7000001"] = nil // no jail role yet: sync must create it
	ops.channel["7000603"] = &fch{ID: "7000603", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	rt := handlerSession(t)

	set(p, panelJail+"sync:", nil)
	if !rt.said("Channels synced") {
		t.Fatal("expected the success reply")
	}
	if _, ok := ops.overwrites[overwriteKey{"7000603", "7900001"}]; !ok {
		t.Fatalf("expected c1 synced against the created jail role, got %v", ops.overwrites)
	}

	ops.guildChannelsErr = transientErr()
	set(p, panelJail+"sync:", nil)
	if !rt.said("Sync completed with errors") {
		t.Fatal("expected the failure reported")
	}
}
