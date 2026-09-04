package aimod

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// The member opt-out, which is the one setting in this package that makes the
// filter cover *less* of a server. Every test here is about a limit rather
// than a feature: the guild switch, the child-safety carve-out, the free
// pattern rungs it does not cover, and who is allowed to set what.

// optOutConfig is an enforcing guild with the switch set and userIDs opted out.
func optOutConfig(on bool, userIDs ...string) Config {
	cfg := enforcingConfig()
	cfg.MemberOptOut = on
	cfg.OptOutUserIDs = userIDs
	return cfg
}

// optOutIntake is intakePlugin with the opt-out applied to the config it
// already stored, so the sealed key it set up survives. Building a fresh
// Config here instead would leave the guild keyless and every "was a model
// called" assertion below would pass for the wrong reason.
func optOutIntake(t *testing.T, store *fakeStore, client *fakeClassifier, on bool, userIDs ...string) *Plugin {
	t.Helper()
	p := intakePlugin(t, store, client, newFakeOps())
	cfg, err := store.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	cfg.MemberOptOut, cfg.OptOutUserIDs = on, userIDs
	store.setConfig(cfg)
	return p
}

// scanExempt is the whole hot-path contract in one function, so it gets a
// table rather than an end-to-end test per row.
func TestScanExemptNeedsBothTheSwitchAndTheMember(t *testing.T) {
	const ordinary = "honestly this whole patch is a mess and i hate it"

	for _, tc := range []struct {
		name    string
		cfg     Config
		author  string
		content string
		want    bool
	}{
		{
			// The default for every deployment that existed before this
			// feature: a stored list read by nothing.
			name: "switch off, member listed", cfg: optOutConfig(false, "u1"),
			author: "u1", content: ordinary, want: false,
		},
		{
			name: "switch on, member not listed", cfg: optOutConfig(true, "u2"),
			author: "u1", content: ordinary, want: false,
		},
		{
			name: "switch on, member listed", cfg: optOutConfig(true, "u1"),
			author: "u1", content: ordinary, want: true,
		},
		{
			// The carve-out. There is no route in this package that turns
			// child_safety off, and an opt-out must not become the third one.
			name: "child safety vocabulary overrides the opt-out", cfg: optOutConfig(true, "u1"),
			author: "u1", content: "anyone here into 14 year old girls", want: false,
		},
		{
			name: "nobody opted out at all", cfg: optOutConfig(true),
			author: "u1", content: ordinary, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanExempt(tc.cfg, tc.author, tc.content); got != tc.want {
				t.Errorf("scanExempt = %v, want %v", got, tc.want)
			}
		})
	}
}

// The saving the feature actually buys: an opted-out member's ordinary
// message is never queued for a model, so it costs nothing.
func TestOptedOutMessageIsNeverQueued(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{}
	p := optOutIntake(t, store, client, true, "u1")

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "honestly this whole patch is a mess and i hate it",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	p.batchMu.Lock()
	queued := len(p.batches)
	p.batchMu.Unlock()
	if queued != 0 {
		t.Errorf("queued %d batches for an opted-out member, want none", queued)
	}
	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("called a model for an opted-out member: fast=%d deep=%d", fast, deep)
	}
}

// Somebody else in the same guild is unaffected, which is what stops a
// passing test above from also passing with the switch wired to the guild.
func TestOptOutAppliesToTheMemberNotTheGuild(t *testing.T) {
	store := newFakeStore()
	p := optOutIntake(t, store, &fakeClassifier{}, true, "u1")

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "honestly this whole patch is a mess and i hate it",
		Author:  &discordgo.User{ID: "u2"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	p.batchMu.Lock()
	queued := len(p.batches)
	p.batchMu.Unlock()
	if queued != 1 {
		t.Errorf("queued %d batches for a member who did not opt out, want 1", queued)
	}
}

// The guard sits *after* rung 1 on purpose. Opting out of a judgement is a
// reasonable thing to want; opting out of a phishing link being deleted is
// not, and the pattern table costs nothing to run.
func TestOptedOutStillGetsTheFreePatternChecks(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	client := &fakeClassifier{}
	p := testPlugin(t, store, client, ops, &fakeAudit{})
	store.setConfig(optOutConfig(true, "u1"))

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "free nitro here https://grabify.link/abcdef",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v, want the grabber link removed despite the opt-out", deleted)
	}
	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("paid for a model on a hard-hit pattern: fast=%d deep=%d", fast, deep)
	}
}

// The carve-out end to end: the one bucket with no off switch anywhere in
// this package does not acquire one here either.
func TestOptedOutChildSafetyVocabularyIsStillScanned(t *testing.T) {
	store := newFakeStore()
	p := optOutIntake(t, store, &fakeClassifier{}, true, "u1")

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: "g1", ChannelID: "c1",
		Content: "anyone here into 14 year old girls",
		Author:  &discordgo.User{ID: "u1"},
		Member:  &discordgo.Member{},
	})
	p.wg.Wait()

	p.batchMu.Lock()
	queued := len(p.batches)
	p.batchMu.Unlock()
	if queued != 1 {
		t.Errorf("queued %d batches, want the child-safety vocabulary scanned regardless of the opt-out", queued)
	}
}

// --- the commands ---

// Opting in to something the guild never offered would leave somebody
// believing they were exempt when they are not, which is the single
// misunderstanding this feature cannot afford.
func TestOptOutRefusedWhereTheGuildHasNotEnabledIt(t *testing.T) {
	store := newFakeStore()
	store.setConfig(optOutConfig(false))
	audit := &fakeAudit{}
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), audit)

	p.handleOptOut(context.Background(), testSession(t), interaction("g1", "", "opt-out", boolOpt("enabled", true)))

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.OptOutUserIDs) != 0 {
		t.Errorf("stored %v with the guild switch off", cfg.OptOutUserIDs)
	}
	if len(audit.actions()) != 0 {
		t.Errorf("audited %v for a refused opt-out", audit.actions())
	}
}

// Removing yourself only ever moves you toward the default, so it is allowed
// even from an inert list. An owner who turns the switch off and on again
// should not resurrect somebody who had already asked to come back.
func TestOptOutRemovalIsAllowedEvenWithTheSwitchOff(t *testing.T) {
	store := newFakeStore()
	store.setConfig(optOutConfig(false, "actor", "u2"))
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.handleOptOut(context.Background(), testSession(t), interaction("g1", "", "opt-out", boolOpt("enabled", false)))

	cfg, _ := store.Config(context.Background(), "g1")
	if got := strings.Join(cfg.OptOutUserIDs, ","); got != "u2" {
		t.Errorf("list is %q, want only u2 left", got)
	}
}

// Consent is not something a third party gives, so the command has no target
// option at all and sets whoever ran it. Asserted on the stored list rather
// than on the handler's shape, because the shape is what a later edit changes.
func TestOptOutSetsOnlyTheCaller(t *testing.T) {
	store := newFakeStore()
	store.setConfig(optOutConfig(true, "u2"))
	audit := &fakeAudit{}
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), audit)

	i := interaction("g1", "", "opt-out", boolOpt("enabled", true))
	i.Member.User.ID = "u9"
	p.handleOptOut(context.Background(), testSession(t), i)

	cfg, _ := store.Config(context.Background(), "g1")
	if got := strings.Join(cfg.OptOutUserIDs, ","); got != "u2,u9" {
		t.Errorf("list is %q, want u2 kept and u9 added", got)
	}
	if got := audit.actions(); len(got) != 1 || got[0] != "aimod.optout_changed" {
		t.Errorf("audited %v, want one aimod.optout_changed", got)
	}
}

// The guild switch is the gate this whole feature hangs off, so the identity
// check is asserted through the handler and against the *stored* value: the
// helper being correct is worth nothing if a later edit stops calling it.
func TestMemberOptOutSwitchIsOwnerOrBootstrapOnly(t *testing.T) {
	// Everybody a TierAdmin PermSpec would admit but this setting must not.
	// /config permissions allow and set-tier can both hand aimod.policy to an
	// ordinary mod, which is exactly why the tier is not the gate.
	for _, actorID := range []string{"actor", "some-admin", "a-mod", "boss-impostor", "own"} {
		store := newFakeStore()
		store.setConfig(optOutConfig(false))
		ops := newFakeOps()
		p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
		p.privilege = fakePrivilege{bootstrapID: "boss"}

		i := interaction("g1", "configure", "member-opt-out", boolOpt("enabled", true))
		i.Member.User.ID = actorID
		p.handleSetMemberOptOut(context.Background(), testSession(t), i)

		cfg, _ := store.Config(context.Background(), "g1")
		if cfg.MemberOptOut {
			t.Errorf("%q turned the member opt-out on", actorID)
		}
	}

	// fakeOps' guild is owned by "owner"; fakePrivilege's bootstrap is "boss".
	for _, actorID := range []string{"owner", "boss"} {
		store := newFakeStore()
		store.setConfig(optOutConfig(false))
		p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
		p.privilege = fakePrivilege{bootstrapID: "boss"}

		i := interaction("g1", "configure", "member-opt-out", boolOpt("enabled", true))
		i.Member.User.ID = actorID
		p.handleSetMemberOptOut(context.Background(), testSession(t), i)

		cfg, _ := store.Config(context.Background(), "g1")
		if !cfg.MemberOptOut {
			t.Errorf("%q could not turn the member opt-out on", actorID)
		}
	}
}

// An unresolvable guild refuses rather than assuming the actor is the owner,
// matching core.Permissions.CanModerate's own fail-closed rule.
func TestMemberOptOutSwitchFailsClosedOnAnUnreadableGuild(t *testing.T) {
	store := newFakeStore()
	store.setConfig(optOutConfig(false))
	ops := newFakeOps()
	ops.guildErr = errors.New("discord is having a day")
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	i := interaction("g1", "configure", "member-opt-out", boolOpt("enabled", true))
	i.Member.User.ID = "owner"
	p.handleSetMemberOptOut(context.Background(), testSession(t), i)

	if cfg, _ := store.Config(context.Background(), "g1"); cfg.MemberOptOut {
		t.Error("turned the opt-out on despite not being able to read who owns the guild")
	}
}

// Turning the switch off stops it applying and keeps the list, so turning it
// back on restores what members chose rather than re-enrolling everybody.
func TestTurningTheSwitchOffKeepsTheList(t *testing.T) {
	store := newFakeStore()
	store.setConfig(optOutConfig(true, "u1", "u2"))
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	i := interaction("g1", "configure", "member-opt-out", boolOpt("enabled", false))
	i.Member.User.ID = "owner"
	p.handleSetMemberOptOut(context.Background(), testSession(t), i)

	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.MemberOptOut {
		t.Fatal("switch is still on")
	}
	if got := strings.Join(cfg.OptOutUserIDs, ","); got != "u1,u2" {
		t.Errorf("list is %q, want it kept intact", got)
	}
	// And the kept list is inert while the switch is off.
	if scanExempt(cfg, "u1", "an ordinary sentence that says nothing much") {
		t.Error("a kept list still exempted somebody with the switch off")
	}
}

// --- the DM ---

// The moderation DM is the one moment this bot has somebody's attention on
// the subject, so it is where the opt-out gets mentioned. Only where the
// guild actually offers it: advertising a command that would refuse is worse
// than saying nothing.
func TestModerationDMMentionsTheOptOutOnlyWhereItIsOffered(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		want bool
	}{
		{name: "offered", on: true, want: true},
		{name: "not offered", on: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newFakeOps()
			p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

			p.notifyAuthor(context.Background(), optOutConfig(tc.on), candidate{
				MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "what i said",
			}, ActionRemove, deepVerdict{Bucket: BucketThreats, Reason: "because"})

			if got := dmMentionsOptOut(ops); got != tc.want {
				t.Errorf("DM mentions the opt-out = %v, want %v", got, tc.want)
			}
		})
	}
}

func dmMentionsOptOut(ops *fakeOps) bool {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	for _, dm := range ops.dms {
		for _, e := range dm.Embeds {
			for _, f := range e.Fields {
				if strings.Contains(f.Value, "/aimod opt-out") {
					return true
				}
			}
		}
	}
	return false
}
