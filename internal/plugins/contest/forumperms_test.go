package contest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordtest"
	"github.com/disgoorg/disgo/discord"
)

// allowOn / denyOn read one target's bits out of a finished overwrite list,
// so an assertion can be about a permission rather than about a slice index.
func allowOn(ow []discord.PermissionOverwrite, id string, kind discord.PermissionOverwriteType) discord.Permissions {
	if e := findIn(ow, id, kind); e != nil {
		return e.Allow
	}
	return 0
}

func denyOn(ow []discord.PermissionOverwrite, id string, kind discord.PermissionOverwriteType) discord.Permissions {
	if e := findIn(ow, id, kind); e != nil {
		return e.Deny
	}
	return 0
}

// findIn is one target's entry in a finished overwrite list, or nil.
func findIn(ow []discord.PermissionOverwrite, id string, kind discord.PermissionOverwriteType) *fow {
	return discordtest.Find(discordtest.Overwrites(ow), id, kind)
}

// The whole point of the mirror: a server states who it is for once, on a
// channel, and the contest forum matches it. Nothing about the role layout is
// stored on merlin's side, so there is nothing to keep in step.
func TestTheGateIsCopiedFromTheChannelItPointsAt(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	seedGate(store, ops)
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(store.cfg["1000"], "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	role := discord.PermissionOverwriteTypeRole
	if denyOn(ow, "1000", role)&discord.PermissionViewChannel == 0 {
		t.Error("@everyone can see a forum in a server where @everyone sees nothing")
	}
	if allowOn(ow, "3001", role)&discord.PermissionViewChannel == 0 {
		t.Error("the role that is what being let in means cannot see the contest")
	}
	if allowOn(ow, "3001", role)&discord.PermissionReadMessageHistory == 0 {
		t.Error("view without history shows a gated member an empty forum")
	}
	if allowOn(ow, botUserID, discord.PermissionOverwriteTypeMember)&discord.PermissionViewChannel == 0 {
		t.Error("merlin cannot see the forum she just made")
	}
}

// The mirror has to work in both directions, or it is a strictness rule
// wearing a mirror's clothes. A server whose general chat is open to everybody
// gets a contest everybody can enter.
func TestAnOpenServerGetsAnOpenContest(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	ops.roles = guildRoles("1000", "3001")
	ops.channels = append(ops.channels, &fch{
		ID: "2005", GuildID: "1000", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{
			{ID: "1000", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel},
		},
	})
	cfg := Config{GuildID: "1000", GateChannelID: "2005"}
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(cfg, "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if denyOn(ow, "1000", discord.PermissionOverwriteTypeRole)&discord.PermissionViewChannel != 0 {
		t.Error("a server where everyone sees everything got a hidden contest forum")
	}
}

// A role named here and deleted since must be dropped rather than written.
// Discord rejects an overwrite for a role that does not exist, and it does not
// reject it small: the whole channel create fails and takes the contest with
// it. This is what lets the role list be stored without a pruning path.
func TestADeletedRoleIsDroppedRatherThanWritten(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	ops.roles = guildRoles("1000", "3001") // "3007" was deleted
	cfg := Config{GuildID: "1000", AccessRoleIDs: []string{"3001", "3007"}}
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(cfg, "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if findIn(ow, "3007", discord.PermissionOverwriteTypeRole) != nil {
		t.Error("an overwrite was written for a role that no longer exists, which fails the whole create")
	}
	if findIn(ow, "3001", discord.PermissionOverwriteTypeRole) == nil {
		t.Error("the surviving role lost its access along with the dead one")
	}
}

// A named list is somebody answering the question outright, so it wins over
// the channel merlin would otherwise read the answer out of.
func TestNamedRolesWinOverTheMirror(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	seedGate(store, ops) // gate-like says "3001"
	cfg := store.cfg["1000"]
	cfg.AccessRoleIDs = []string{"3002"}
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(cfg, "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	role := discord.PermissionOverwriteTypeRole
	if findIn(ow, "3001", role) != nil {
		t.Error("the mirror was applied on top of an explicit list")
	}
	if allowOn(ow, "3002", role)&discord.PermissionViewChannel == 0 {
		t.Error("the named role was not granted access")
	}
}

// A media role grants attachment rights and nothing else, because the mirror
// deliberately copies only view: a text channel's SendMessages does not mean
// the same thing as a forum's CreatePublicThreads.
func TestAMediaRoleGrantsAttachmentsOnly(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	ops.roles = guildRoles("1000", "3001", "3004")
	cfg := Config{GuildID: "1000", AccessRoleIDs: []string{"3001"}, MediaRoleIDs: []string{"3004"}}
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(cfg, "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	role := discord.PermissionOverwriteTypeRole
	if allowOn(ow, "3004", role)&discord.PermissionAttachFiles == 0 {
		t.Error("the media role cannot attach files, so entries have no art")
	}
	if allowOn(ow, "3004", role)&discord.PermissionViewChannel != 0 {
		t.Error("a media role was quietly granted access as well")
	}
}

// A deny somebody else set is none of this code's business. A jailed role's
// deny on a contest forum is a decision a moderator made, and a resync that
// tidied it away would let a jailed member back into the contest.
func TestSomebodyElsesDenyIsLeftAlone(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	seedGate(store, ops)
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	current := []*fow{{
		ID: "3005", Type: discord.PermissionOverwriteTypeRole,
		Deny: discord.PermissionViewChannel,
	}}
	ow, err := p.forumOverwritesFor(store.cfg["1000"], "1000", (&fch{PermissionOverwrites: current}).Disgo().PermissionOverwrites(), false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if denyOn(ow, "3005", discord.PermissionOverwriteTypeRole)&discord.PermissionViewChannel == 0 {
		t.Error("a jailed role's deny was tidied away, letting a jailed member into the contest")
	}
}

// Applying the result to its own output has to change nothing, or sync-forum
// writes on every run and every write lands in the guild's Discord audit log.
func TestTheOverwriteListIsIdempotent(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	seedGate(store, ops)
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	once, err := p.forumOverwritesFor(store.cfg["1000"], "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	twice, err := p.forumOverwritesFor(store.cfg["1000"], "1000", once, false)
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if !core.SameOverwrites(once, twice) {
		t.Errorf("applying the gate to itself changed it:\n%+v\n%+v", once, twice)
	}
}

// Opening and closing may only ever move the posting bit. This is the same
// property the previous PR gave setForumOpen, asserted here for the builder
// the resync goes through, because that is the other way a phase can reach
// the permission list.
func TestOpeningOnlyMovesThePostingBit(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	seedGate(store, ops)
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	shut, err := p.forumOverwritesFor(store.cfg["1000"], "1000", nil, false)
	if err != nil {
		t.Fatalf("shut: %v", err)
	}
	open, err := p.forumOverwritesFor(store.cfg["1000"], "1000", nil, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	role := discord.PermissionOverwriteTypeRole
	if denyOn(open, "1000", role)&postingPerms != 0 {
		t.Error("an open forum still denies posting")
	}
	if denyOn(shut, "1000", role)&postingPerms == 0 {
		t.Error("a shut forum allows posting")
	}
	// Everything except the posting bit is identical between the two.
	for _, id := range []string{"1000", "3001"} {
		if allowOn(shut, id, role) != allowOn(open, id, role) {
			t.Errorf("%s gained or lost an allow when the phase changed", id)
		}
		if denyOn(shut, id, role)&^postingPerms != denyOn(open, id, role)&^postingPerms {
			t.Errorf("%s gained or lost a deny when the phase changed", id)
		}
	}
}

// /contest new refuses rather than guessing, because the guess that is wrong
// publishes members' work to accounts the server deliberately did not let in,
// and nobody finds out until it has happened.
func TestNewRefusesUntilSomebodySaysWhoContestsAreFor(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	ops.roles = guildRoles("1000", "3001")
	p := newTestPlugin(t, store, ops, sched, audit, "")
	rt := stubSession()

	p.handleNew(context.Background(), interaction("new", strOpt("title", "neon cats")))

	if _, err := store.LiveContest(context.Background(), "1000"); !errors.Is(err, ErrNoLiveContest) {
		t.Error("an ungated guild started a contest anyway")
	}
	if len(ops.created) != 0 {
		t.Error("an ungated guild got a forum channel")
	}
	said := rt.said()
	if !strings.Contains(said, "/contest settings") || !strings.Contains(said, "roles outright") {
		t.Errorf("the refusal does not name the way out of it: %s", said)
	}
}

// An unreadable reference channel is not permission to fall back to open.
// It says nothing about who should be let in, and the wrong guess in that
// direction is the one that cannot be taken back.
func TestAnUnreadableGateChannelStopsTheContest(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	ops.roles = guildRoles("1000", "3001")
	// gate-like points at a channel the fake does not have and cannot read.
	store.cfg["1000"] = Config{GuildID: "1000", DefaultMaxVotes: 2, GateChannelID: "2009"}
	ops.channelErr = errors.New("404 unknown channel")
	p := newTestPlugin(t, store, ops, sched, audit, "")
	stubSession()

	p.handleNew(context.Background(), interaction("new", strOpt("title", "neon cats")))

	if len(ops.created) != 0 {
		t.Error("a forum was created without knowing who it is for")
	}
	if _, err := store.LiveContest(context.Background(), "1000"); !errors.Is(err, ErrNoLiveContest) {
		t.Error("a contest survived a failed gate resolution, so it will collect nothing forever")
	}
}

// Configuring the gate while a contest is running has to be able to reach the
// forum that already exists, or the only way to shut an open one is to cancel
// the contest.
func TestSyncForumAppliesTheGateToARunningContest(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedGate(store, ops)
	c := liveContest(PhaseSubmit, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The forum as an older merlin left it: open to the whole server.
	ops.channels = append(ops.channels, &fch{
		ID: c.ForumChannelID, GuildID: "1000", Type: discord.ChannelTypeGuildForum,
	})
	p := newTestPlugin(t, store, ops, sched, audit, "")
	stubSession()

	i := decodeIx(3, "4001", "4001", map[string]any{"custom_id": panelSet + "sync", "component_type": 2})
	p.handlePanelSet(context.Background(), i, panelSet+"sync")

	got, _ := ops.GetChannel(core.ID(c.ForumChannelID))
	ow := got.(discord.GuildChannel).PermissionOverwrites()
	if denyOn(ow, "1000", discord.PermissionOverwriteTypeRole)&discord.PermissionViewChannel == 0 {
		t.Error("the running contest's forum is still visible to the whole server")
	}
	// Submissions are open, so the resync must not have shut them as a side
	// effect of fixing who can see the channel.
	if denyOn(ow, "1000", discord.PermissionOverwriteTypeRole)&postingPerms != 0 {
		t.Error("the resync closed submissions on a contest that was taking entries")
	}
}

// And a resync with nothing to do writes nothing, because every permission
// write lands in the guild's own Discord audit log and a no-op one buries the
// entries a moderator is looking for.
func TestSyncForumWritesNothingWhenAlreadyRight(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	seedGate(store, ops)
	c := liveContest(PhaseSubmit, base)
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ops.channels = append(ops.channels, &fch{
		ID: c.ForumChannelID, GuildID: "1000", Type: discord.ChannelTypeGuildForum,
	})
	p := newTestPlugin(t, store, ops, sched, audit, "")

	if _, err := p.syncForum(store.cfg["1000"], c); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := ops.edits
	changed, err := p.syncForum(store.cfg["1000"], c)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if changed || ops.edits != before {
		t.Error("a resync that had nothing to change wrote to Discord anyway")
	}
}

// --- the mirror reads permissions, not the overwrite list -----------------

// The gate that actually exists on most servers: @everyone has no View
// Channels at the guild level, a role granted on verification does, and the
// gated channel itself carries no overwrites at all because it does not need
// any. Scraping the overwrite list found nothing there and produced a forum
// denied to @everyone and granted to nobody, so the only accounts that could
// see a contest were merlin and the guild's admins.
func TestAGateMadeOfRolePermissionsIsMirrored(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	ops.roles = guildRoles("1000", "3001", "3006")
	roleByID(ops.roles, "3001").Permissions = discord.PermissionViewChannel
	ops.channels = append(ops.channels, &fch{
		ID: "2005", GuildID: "1000", Type: discord.ChannelTypeGuildText,
	})
	cfg := Config{GuildID: "1000", GateChannelID: "2005"}
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(cfg, "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	role := discord.PermissionOverwriteTypeRole
	if denyOn(ow, "1000", role)&discord.PermissionViewChannel == 0 {
		t.Error("@everyone can see a forum in a server where @everyone sees nothing")
	}
	if allowOn(ow, "3001", role)&discord.PermissionViewChannel == 0 {
		t.Error("the role that is what being let in means cannot see the contest")
	}
	if findIn(ow, "3006", role) != nil {
		t.Error("a role that cannot see the gate channel was let into the contest")
	}
}

// The same read, failing the other way: a channel everybody can see because
// the guild default says so and nothing overrides it. An absent @everyone
// entry was taken as "everyone may not view", so an open server got a forum
// its members could not find.
func TestAChannelOpenByDefaultIsMirroredAsOpen(t *testing.T) {
	store, ops := newFakeStore(), newFakeOps()
	ops.roles = guildRoles("1000", "3001")
	roleByID(ops.roles, "1000").Permissions = discord.PermissionViewChannel
	ops.channels = append(ops.channels, &fch{
		ID: "2005", GuildID: "1000", Type: discord.ChannelTypeGuildText,
	})
	cfg := Config{GuildID: "1000", GateChannelID: "2005"}
	p := newTestPlugin(t, store, ops, newFakeSched(), &fakeAudit{}, "")

	ow, err := p.forumOverwritesFor(cfg, "1000", nil, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if denyOn(ow, "1000", discord.PermissionOverwriteTypeRole)&discord.PermissionViewChannel != 0 {
		t.Error("a server where everyone sees everything got a hidden contest forum")
	}
	if findIn(ow, "3001", discord.PermissionOverwriteTypeRole) != nil {
		t.Error("an entry per role on a forum everyone can already see, for nothing")
	}
}
