package aimod

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// sanctionFor decides how long a real person loses access to a community
// for, which is why it is pure and why its whole behaviour is asserted off a
// table rather than inferred from a couple of spot checks.

func TestSanctionForScalesWithSeverityAndHistory(t *testing.T) {
	tests := []struct {
		severity string
		priors   int
		want     time.Duration
	}{
		// First offence: the base for each tier.
		{"critical", 0, 24 * time.Hour},
		{"high", 0, 8 * time.Hour},
		{"medium", 0, 2 * time.Hour},
		{"low", 0, 30 * time.Minute},

		// Each prior inside the window doubles it.
		{"low", 1, time.Hour},
		{"low", 2, 2 * time.Hour},
		{"low", 3, 4 * time.Hour},
		{"high", 1, 16 * time.Hour},
		{"high", 2, 32 * time.Hour},

		// Doubling stops at maxRepeatDoublings, so a bad month cannot
		// arithmetic its way to a permanent ban.
		{"low", 4, 4 * time.Hour},
		{"low", 40, 4 * time.Hour},

		// And the whole thing is capped regardless.
		{"critical", 3, maxSanction},
		{"critical", 99, maxSanction},

		// A missing or unrecognised severity lands mid-ladder, not at the
		// top: a configuration gap must not hand somebody a day in jail.
		{"", 0, unknownSeverityBase},
		{"catastrophic", 0, unknownSeverityBase},

		// A negative count can only come from arithmetic going wrong
		// upstream, and must not shorten a sentence below its base.
		{"high", -3, 8 * time.Hour},
	}

	for _, tc := range tests {
		if got := sanctionFor(tc.severity, tc.priors); got != tc.want {
			t.Errorf("sanctionFor(%q, %d) = %s, want %s", tc.severity, tc.priors, got, tc.want)
		}
	}
}

// Every sentence gets longer, never shorter, as the history grows. Stated as
// a property rather than a row, because the failure it guards against is an
// off-by-one somewhere in the shift that a table of examples can step over.
func TestSanctionsAreMonotonicInHistory(t *testing.T) {
	for severity := range severityBase {
		prev := time.Duration(0)
		for priors := range 8 {
			got := sanctionFor(severity, priors)
			if got < prev {
				t.Errorf("%s: %d priors gave %s, less than the %s at %d priors", severity, priors, got, prev, priors-1)
			}
			if got > maxSanction {
				t.Errorf("%s: %d priors gave %s, past the %s cap", severity, priors, got, maxSanction)
			}
			prev = got
		}
	}
}

func sanctioningConfig() Config {
	cfg := enforcingConfig()
	cfg.SanctionAction = SanctionJail
	return cfg
}

func TestSanctionJailsThroughTheJailer(t *testing.T) {
	store := newFakeStore()
	jailer := &fakeJailer{}
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.jailer = jailer

	p.sanction(context.Background(), sanctioningConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketThreats, "a specific threat")

	jailed := jailer.jailed()
	if len(jailed) != 1 {
		t.Fatalf("jailed %d members, want 1", len(jailed))
	}
	// threats is critical in the catalogue, and this is a first offence.
	if jailed[0].duration != 24*time.Hour {
		t.Errorf("duration = %s, want 24h for a first critical offence", jailed[0].duration)
	}
	if jailed[0].consented {
		t.Error("the consent flag was set for a member who never opted in")
	}
}

// The sanction row is what the next offence counts, so it has to be written
// even when the jail itself could not be applied. Losing it because Discord
// was briefly unreachable would quietly reset somebody's history to zero.
func TestSanctionIsRecordedEvenWhenTheJailFails(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	audit := &fakeAudit{}
	p := testPlugin(t, store, &fakeClassifier{}, ops, audit)
	p.jailer = &fakeJailer{err: errors.New("role hierarchy")}

	p.sanction(context.Background(), sanctioningConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketThreats, "reason")

	var sanctions int
	for _, inc := range store.recorded() {
		if inc.Action == ActionSanction {
			sanctions++
		}
	}
	if sanctions != 1 {
		t.Errorf("recorded %d sanction rows after a failed jail, want 1", sanctions)
	}
	if !contains(audit.actions(), "aimod.sanctioned") {
		t.Error("no audit entry, so nobody is told the jail was wanted and did not happen")
	}
}

// The history that drives the ladder counts prior sanctions, so a member on
// their second confirmed violation gets double.
func TestRepeatOffenceDoublesTheSentence(t *testing.T) {
	store := newFakeStore()
	jailer := &fakeJailer{}
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.jailer = jailer
	cfg := sanctioningConfig()

	p.sanction(context.Background(), cfg, candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketGore, "first")
	p.sanction(context.Background(), cfg, candidate{MessageID: "m2", ChannelID: "c1", AuthorID: "u1"}, BucketGore, "second")

	jailed := jailer.jailed()
	if len(jailed) != 2 {
		t.Fatalf("jailed %d times, want 2", len(jailed))
	}
	if jailed[1].duration != jailed[0].duration*2 {
		t.Errorf("second sentence %s, want double the first (%s)", jailed[1].duration, jailed[0].duration)
	}
}

// A different member's history is not this member's history.
func TestHistoryIsPerMember(t *testing.T) {
	store := newFakeStore()
	jailer := &fakeJailer{}
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.jailer = jailer
	cfg := sanctioningConfig()

	p.sanction(context.Background(), cfg, candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketGore, "r")
	p.sanction(context.Background(), cfg, candidate{MessageID: "m2", ChannelID: "c1", AuthorID: "u2"}, BucketGore, "r")

	jailed := jailer.jailed()
	if jailed[0].duration != jailed[1].duration {
		t.Errorf("a second member got %s where the first got %s: one member's history leaked into another's",
			jailed[1].duration, jailed[0].duration)
	}
}

// A guild that has not asked for automatic jails does not get them, and an
// unrecognised stored value counts as not having asked.
func TestSanctionRequiresExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"sanctions off", func(c *Config) { c.SanctionAction = SanctionOff }},
		{"sanctions flag only", func(c *Config) { c.SanctionAction = SanctionFlag }},
		{"unrecognised stored value", func(c *Config) { c.SanctionAction = SanctionAction("timeout") }},
		{"empty stored value", func(c *Config) { c.SanctionAction = "" }},
		{"guild is only flagging", func(c *Config) { c.Mode = ModeFlag }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jailer := &fakeJailer{}
			p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
			p.jailer = jailer

			cfg := sanctioningConfig()
			tc.mutate(&cfg)
			p.sanction(context.Background(), cfg, candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketThreats, "r")

			if len(jailer.jailed()) != 0 {
				t.Error("jailed somebody in a guild that never asked for automatic jails")
			}
		})
	}
}

// Jail is the primary mechanism; the Discord timeout exists for when jail is
// genuinely unavailable, and must not be reached while jail is working.
func TestTimeoutIsOnlyReachedWhenJailIsUnavailable(t *testing.T) {
	t.Run("jail works, no timeout", func(t *testing.T) {
		ops := newFakeOps()
		p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})
		p.jailer = &fakeJailer{}

		p.sanction(context.Background(), sanctioningConfig(),
			candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketThreats, "r")

		if len(ops.timeouts) != 0 {
			t.Errorf("timed out %v while jail was working", ops.timeouts)
		}
	})

	t.Run("no jailer at all falls back", func(t *testing.T) {
		ops := newFakeOps()
		p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})
		p.jailer = nil

		p.sanction(context.Background(), sanctioningConfig(),
			candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1"}, BucketThreats, "r")

		if _, ok := ops.timeouts["u1"]; !ok {
			t.Error("no jailer and no timeout: the member walked away with nothing")
		}
	})
}

// Discord caps a timeout at 28 days. The ladder can compute longer, and a
// request past the cap is rejected outright rather than clamped by Discord,
// so it is clamped here.
func TestTimeoutIsClampedToDiscordsCeiling(t *testing.T) {
	ops := newFakeOps()
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	if err := p.timeoutMember(context.Background(), "g1", "u1", 60*24*time.Hour, false); err != nil {
		t.Fatalf("timeoutMember: %v", err)
	}
	if got := ops.timeouts["u1"]; got > maxDiscordTimeout {
		t.Errorf("requested a %s timeout, past Discord's %s ceiling", got, maxDiscordTimeout)
	}
}

// The automatic path must never be aimable at a guild's staff. This is the
// same gap core.Permissions.CanModerate closes for /roles jail, arrived at
// from the other side.
func TestTimeoutRefusesStaffAndOwner(t *testing.T) {
	ops := newFakeOps()
	ops.guild.OwnerID = "owner"
	ops.guild.Roles = []*discordgo.Role{
		{ID: "r-mod", Permissions: discordgo.PermissionManageMessages},
		{ID: "r-member", Permissions: 0},
	}
	ops.members["mod"] = &discordgo.Member{User: &discordgo.User{ID: "mod"}, Roles: []string{"r-mod"}}
	ops.members["member"] = &discordgo.Member{User: &discordgo.User{ID: "member"}, Roles: []string{"r-member"}}
	ops.members["ghost"] = &discordgo.Member{User: &discordgo.User{ID: "ghost"}, Roles: []string{"r-vanished"}}

	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	for _, userID := range []string{"owner", "mod"} {
		if err := p.timeoutMember(context.Background(), "g1", userID, time.Hour, false); err == nil {
			t.Errorf("timed out %q, who holds a role this bot must not aim at", userID)
		}
	}
	// A role the guild read did not include cannot be cleared, and clearing
	// it by default is how the carve-out gets bypassed.
	if err := p.timeoutMember(context.Background(), "g1", "ghost", time.Hour, false); err == nil {
		t.Error("timed out a member whose roles could not be fully resolved")
	}
	if err := p.timeoutMember(context.Background(), "g1", "member", time.Hour, false); err != nil {
		t.Errorf("refused an ordinary member: %v", err)
	}
}

// Consent waives the staff-rank refusal, which is the entire point of the
// opt-in list, but never the owner carve-out, which Discord enforces anyway.
func TestConsentWaivesTheStaffRefusalButNotTheOwner(t *testing.T) {
	ops := newFakeOps()
	ops.guild.OwnerID = "owner"
	ops.guild.Roles = []*discordgo.Role{{ID: "r-mod", Permissions: discordgo.PermissionManageMessages}}
	ops.members["mod"] = &discordgo.Member{User: &discordgo.User{ID: "mod"}, Roles: []string{"r-mod"}}

	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	if err := p.timeoutMember(context.Background(), "g1", "mod", time.Hour, true); err != nil {
		t.Errorf("a moderator who opted in was still refused: %v", err)
	}
	if err := p.timeoutMember(context.Background(), "g1", "owner", time.Hour, true); err == nil {
		t.Error("timed out the guild owner, which Discord rejects regardless of consent")
	}
}

// The opt-in list is purely additive: it widens who can be sanctioned and
// never narrows it. If this inverts, it becomes a way to buy immunity.
func TestOptInListOnlyWidens(t *testing.T) {
	cfg := Config{SanctionOptInUserIDs: []string{"u1"}}
	if !sanctionable(cfg, "u1") {
		t.Error("a member on the list is not sanctionable")
	}
	if sanctionable(cfg, "u2") {
		t.Error("a member not on the list was reported as opted in")
	}
	if sanctionable(Config{}, "u1") {
		t.Error("an empty list reported somebody as opted in")
	}
}

// Only the bootstrap operator manages other people's entries. Everyone else,
// at every tier, is limited to opting themselves in.
func TestOnlyTheOperatorManagesOtherPeoplesOptIns(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.privilege = fakePrivilege{bootstrapID: "boot"}

	if !p.canManageOptIns("boot") {
		t.Error("the bootstrap operator cannot manage opt-ins")
	}
	for _, id := range []string{"some-admin", "some-mod", "", "BOOT"} {
		if p.canManageOptIns(id) {
			t.Errorf("%q can manage other people's opt-ins", id)
		}
	}

	// With no privilege checker wired in, nobody can, rather than everybody.
	p.privilege = nil
	if p.canManageOptIns("boot") {
		t.Error("an unwired privilege checker granted the override instead of refusing it")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
