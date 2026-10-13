package aimod

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

// The slash-command handlers, driven directly.
//
// They are most of this package's statements and were entirely untested,
// which matters more than the number suggests: these are where an admin's
// intent becomes a stored setting, and several of them carry an invariant
// that is invisible from the store's side (a mode that refuses to enable
// without a key, an opt-in list only the bootstrap operator may add somebody
// else to, a proposal re-validated against live config before it applies).
//
// What makes them testable is an interaction decoded from Discord's JSON whose
// REST client answers every call with an empty success (paneltest.Rest). The
// handler runs to completion, its replies go nowhere, and the assertions are
// about what it did rather than what it said. No network, no Discord, no
// fixture server.

// interaction builds a command interaction for one leaf of /aimod, from
// "actor" in guildID.
//
// path is the subcommand group and subcommand ("calibrate", "mode"), matching
// how CommandRouter.Handle names them, so a test reads the same way the
// registration does. It is Discord's JSON decoded by core.NewIx, as the
// router decodes a real one.
func interaction(guildID, group, sub string, args ...map[string]any) *core.Ix {
	ix, _ := interactionWire(guildID, group, sub, args...)
	return ix
}

// interactionWire is interaction and the wire its answer lands on.
func interactionWire(guildID, group, sub string, args ...map[string]any) (*core.Ix, *paneltest.Wire) {
	opts := []any{}
	for _, a := range args {
		opts = append(opts, a)
	}
	leaf := map[string]any{"name": sub, "type": 1, "options": opts}
	top := []any{leaf}
	if group != "" {
		top = []any{map[string]any{"name": group, "type": 2, "options": []any{leaf}}}
	}
	return decodeIx(guildID, 2, map[string]any{"id": "6", "name": "aimod", "type": 1, "options": top}, nil)
}

// decodeIx is Discord's JSON for an interaction of typ decoded by core.NewIx,
// answered through a REST client that records instead of sending.
//
// These tests' fixtures predate disgo: the store and fakes key on "g1" and
// "actor", which are not snowflakes. Ix.GuildID and Ix.UserID are merlin's
// own string keys and every handler reads those, so after decoding a real
// interaction they are pointed at the fixtures.
func decodeIx(guildID string, typ int, data, message map[string]any) (*core.Ix, *paneltest.Wire) {
	p := map[string]any{
		"id": "1000", "application_id": "2", "type": typ, "token": "t", "version": 1,
		"guild_id": paneltest.IxGuild,
		"channel":  map[string]any{"id": "101", "type": 0},
		"member": map[string]any{
			"user":        map[string]any{"id": "11", "username": "actor"},
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
	r, wire := paneltest.Rest()
	ix, err := core.NewIx(raw, r)
	if err != nil {
		panic(err)
	}
	ix.GuildID, ix.UserID = guildID, "actor"
	return ix, wire
}

func strOpt(name, value string) map[string]any {
	return map[string]any{"name": name, "type": 3, "value": value}
}

// The whole command tree, registered through this plugin's own Init and then
// finalized. Finalize is where a leaf with no declared tier or a subcommand
// with no handler fails the build, so this is the test that stops a new
// /aimod leaf reaching a live server half-wired. Mirrors adminconfig's.
func TestInitRegistersAFullyWiredCommandTree(t *testing.T) {
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	perms := core.NewPermissions(nil, nil, "")
	router := core.NewCommandRouter(perms, nil, log)

	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.commands = router
	p.registerCommands()

	if err := router.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// The mode ladder's guard: enabling with no API key configured leaves a
// guild believing it is protected while only the pattern rungs run.
func TestModeRefusesToEnableWithoutAKey(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setMode(context.Background(), interaction("g1", "", "settings"), Mode("enforce"))

	cfg, err := store.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Mode != ModeOff {
		t.Errorf("mode = %q, want off: enabling with no key was allowed", cfg.Mode)
	}
}

func TestModeSetsWithAKeyConfigured(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	cfg := enforcingConfig()
	cfg.Mode = ModeOff
	cfg.APIKeySealed = []byte("sealed")
	store.setConfig(cfg)

	p.setMode(context.Background(), interaction("g1", "", "settings"), Mode("flag"))

	got, err := store.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got.Mode != ModeFlag {
		t.Errorf("mode = %q, want flag", got.Mode)
	}
}

// An unknown mode is rejected rather than stored, because the column has no
// CHECK for this one and a stored nonsense value would read as off.
func TestUnknownModeIsRejected(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setMode(context.Background(), interaction("g1", "", "settings"), Mode("sideways"))

	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.Mode == "sideways" {
		t.Error("an invented mode was stored")
	}
}

// child_safety is not disableable, the same guard shape as adminconfig
// refusing to disable itself: there is no legitimate reason to turn it off,
// so there is no way to.
func TestChildSafetyCannotBeSwitchedOff(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setPolicy(context.Background(), interaction("g1", "", "settings"), Bucket(string(BucketChildSafety)), Action(string(ActionOff)))

	cfg, _ := store.Config(context.Background(), "g1")
	if got := EffectiveAction(cfg.BucketActions, BucketChildSafety); got != ActionRemove {
		t.Errorf("child_safety = %q, want remove regardless of what was stored", got)
	}
}

func TestPolicySetStoresAValidAction(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setPolicy(context.Background(), interaction("g1", "", "settings"), Bucket(string(BucketHateSpeech)), Action(string(ActionRewrite)))

	cfg, _ := store.Config(context.Background(), "g1")
	if got := EffectiveAction(cfg.BucketActions, BucketHateSpeech); got != ActionRewrite {
		t.Errorf("hate_speech = %q, want rewrite", got)
	}
}

// /aimod calibrate mode is the switch that registers or unregisters the
// weekly job, and doing that is its whole effect: this plugin has no
// EventConfigChanged subscription, so a handler that stored the value without
// reconciling would leave a guild with a setting that did nothing.
func TestCalibrateModeStoresAndReconciles(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.CalibrationMode = CalibrationOff
	store.setConfig(cfg)

	sched := &fakeScheduler{jobs: map[string]bool{}}
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.sched = sched

	p.setCalibrationMode(context.Background(), interaction("g1", "", "settings"), CalibrationMode(string(CalibrationAuto)))

	got, _ := store.Config(context.Background(), "g1")
	if got.CalibrationMode != CalibrationAuto {
		t.Errorf("calibration mode = %q, want auto", got.CalibrationMode)
	}
	if !sched.jobs[calibrationJobKey("g1")] {
		t.Error("the weekly job was not registered, so the setting does nothing")
	}

	p.setCalibrationMode(context.Background(), interaction("g1", "", "settings"), CalibrationMode(string(CalibrationOff)))
	if sched.jobs[calibrationJobKey("g1")] {
		t.Error("switching reviews off left the job registered")
	}
}

func TestCalibrateModeRejectsAnUnknownValue(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setCalibrationMode(context.Background(), interaction("g1", "", "settings"), CalibrationMode("whenever"))

	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.CalibrationMode == "whenever" {
		t.Error("an invented calibration mode was stored")
	}
}

// Apply re-validates the proposal against live config rather than trusting
// the column. A proposal can sit for a week, and a bucket switched off in the
// meantime must not come back into force through a stored example.
func TestCalibrateApplyRevalidatesAgainstLiveConfig(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.BucketActions[BucketHateSpeech] = ActionOff
	cfg.CalibrationPending = []CalibrationExample{
		{Text: "still fine here", Bucket: BucketThreats},
		{Text: "about a bucket since switched off", Bucket: BucketHateSpeech},
	}
	store.setConfig(cfg)

	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.calibrateApply(context.Background(), interaction("g1", "", "settings"))

	got, _ := store.Config(context.Background(), "g1")
	if len(got.Calibration) != 1 || got.Calibration[0].Bucket != BucketThreats {
		t.Errorf("applied %+v, want only the example for a bucket still enforced", got.Calibration)
	}
	if len(got.CalibrationPending) != 0 {
		t.Error("the proposal survived being applied, so it can be applied twice")
	}
}

// Applying nothing must not clear what is already in force. The failure it
// guards is silent: a guild's calibration quietly emptying because somebody
// ran apply twice.
func TestCalibrateApplyWithNothingPendingChangesNothing(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.Calibration = []CalibrationExample{{Text: "in force", Bucket: BucketThreats}}
	store.setConfig(cfg)

	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.calibrateApply(context.Background(), interaction("g1", "", "settings"))

	got, _ := store.Config(context.Background(), "g1")
	if len(got.Calibration) != 1 {
		t.Errorf("active set is now %+v: applying nothing wiped it", got.Calibration)
	}
}

func TestCalibrateClearEmptiesBothSets(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.Calibration = []CalibrationExample{{Text: "in force", Bucket: BucketThreats}}
	cfg.CalibrationPending = []CalibrationExample{{Text: "proposed", Bucket: BucketThreats}}
	store.setConfig(cfg)

	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.calibrateClear(context.Background(), interaction("g1", "", "settings"))

	got, _ := store.Config(context.Background(), "g1")
	if len(got.Calibration) != 0 || len(got.CalibrationPending) != 0 {
		t.Errorf("clear left %d active and %d pending", len(got.Calibration), len(got.CalibrationPending))
	}
}

// show has to render for a guild that has never been reviewed as readily as
// for one carrying a full set. It is the command a moderator reaches for when
// they want to know why a message was left alone, so it failing on the empty
// case would be the worst time for it.
func TestCalibrateShowRendersEmptyAndPopulated(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(Config) Config
	}{
		{"never reviewed", func(c Config) Config { return c }},
		{"with a set and a proposal", func(c Config) Config {
			c.Calibration = []CalibrationExample{{Text: "in force", Bucket: BucketThreats, Note: "n"}}
			c.CalibrationPending = []CalibrationExample{{Text: "proposed", Bucket: BucketThreats, ShouldAct: true}}
			return c
		}},
		{"evidence retention off", func(c Config) Config { c.EvidenceHours = 0; return c }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.setConfig(tc.cfg(enforcingConfig()))
			p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
			p.handleCalibrateShow(context.Background(), interaction("g1", "calibrate", "show"))
		})
	}
}

// run-now on a guild that is switched off has no history to learn from, and
// must say so rather than paying a model to invent norms for a server it has
// not watched.
func TestCalibrateRunNowRefusesWhenThePluginIsOff(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.Mode = ModeOff
	store.setConfig(cfg)

	client := &fakeClassifier{}
	p := testPlugin(t, store, client, newFakeOps(), &fakeAudit{})
	p.calibrateNow(context.Background(), interaction("g1", "", "settings"))

	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("paid for a review on a disabled guild: fast=%d deep=%d", fast, deep)
	}
}

// A quiet week is an ordinary outcome. Returning an error for it would back
// the weekly job off and eventually alert on a guild that is simply peaceful.
func TestCalibrateRunNowReportsAQuietWeek(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	client := &fakeClassifier{}
	p := testPlugin(t, store, client, newFakeOps(), &fakeAudit{})

	p.calibrateNow(context.Background(), interaction("g1", "", "settings"))

	if fast, deep := client.counts(); fast != 0 || deep != 0 {
		t.Errorf("paid for a review with no incidents to review: fast=%d deep=%d", fast, deep)
	}
}

// The opt-in list only ever widens who can be sanctioned, and anybody may add
// themselves. That is what makes it safe: nothing reads it to decide whether
// to protect anyone.
func TestModerateMeAddsAndRemovesTheCaller(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	on := interaction("g1", "", "moderate-me", boolOpt("enabled", true))
	p.handleModerateMe(context.Background(), on)

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.SanctionOptInUserIDs) != 1 || cfg.SanctionOptInUserIDs[0] != "actor" {
		t.Errorf("opt-in list = %v, want the caller added", cfg.SanctionOptInUserIDs)
	}

	off := interaction("g1", "", "moderate-me", boolOpt("enabled", false))
	p.handleModerateMe(context.Background(), off)

	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.SanctionOptInUserIDs) != 0 {
		t.Errorf("opt-in list = %v, want the caller removed", cfg.SanctionOptInUserIDs)
	}
}

// Adding somebody else is the bootstrap operator's alone. A mod who could
// opt a colleague in would have found a way to make an automated sanction
// aimable at staff, which is the one thing the rank check exists to stop.
func TestModerateUserIsBootstrapOnly(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.privilege = fakePrivilege{bootstrapID: "somebody-else"}

	i := interaction("g1", "", "moderate-user",
		map[string]any{"name": "user", "type": 6, "value": "victim"},
		boolOpt("enabled", true))
	p.handleModerateUser(context.Background(), i)

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.SanctionOptInUserIDs) != 0 {
		t.Errorf("a non-bootstrap caller opted somebody else in: %v", cfg.SanctionOptInUserIDs)
	}

	p.privilege = fakePrivilege{bootstrapID: "actor"}
	p.handleModerateUser(context.Background(), i)
	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.SanctionOptInUserIDs) != 1 {
		t.Errorf("the bootstrap operator could not opt somebody in: %v", cfg.SanctionOptInUserIDs)
	}
}

// /aimod why on a message this plugin never touched, which is the common
// case: a mod checking whether the filter was involved at all.
func TestWhyOnAnUntouchedMessage(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.handleWhy(context.Background(), interaction("g1", "", "why", strOpt("id", "m-unknown")))
}

// /aimod why is public because the code is printed under the repost. What a
// member gets back is the policy and the reason, never the original text
// (for a rewrite, the very words taken out of the channel), and a message ID
// gets them nothing, or any member could probe whether a message had been
// quietly flagged. A moderator gets the whole record by either key.
func TestWhyShowsAMemberOnlyThePublicHalf(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.privilege = fakePrivilege{bootstrapID: "boss"}
	ctx := context.Background()
	if _, err := store.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1", Bucket: BucketHateSpeech,
		Action: ActionRewrite, Reason: "hard slur", Content: "the original words", Code: "abcd2345",
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}

	ask := func(actor, ref string) string {
		i, wire := interactionWire("g1", "", "why", strOpt("id", ref))
		i.UserID = actor
		p.handleWhy(ctx, i)
		return wire.All()
	}

	got := ask("member", "ABCD2345")
	if !strings.Contains(got, "hard slur") {
		t.Errorf("a member with the code was not told why: %s", got)
	}
	if strings.Contains(got, "the original words") {
		t.Errorf("a member was shown the original: %s", got)
	}
	if got := ask("member", "m1"); strings.Contains(got, "hard slur") {
		t.Errorf("a member looked an incident up by message ID: %s", got)
	}
	for _, ref := range []string{"abcd2345", "m1"} {
		if got := ask("boss", ref); !strings.Contains(got, "the original words") {
			t.Errorf("a moderator asking with %q was not shown the original: %s", ref, got)
		}
	}
}

// /aimod status is the command an admin runs when they think something is
// wrong, so it has to render on a guild where everything is unset.
func TestStatusRendersOnAnUnconfiguredGuild(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.handleStatus(context.Background(), interaction("g1", "", "status"))
}

func boolOpt(name string, v bool) map[string]any {
	return map[string]any{"name": name, "type": 5, "value": v}
}

// The daily cap is the only thing between a misconfigured guild and an
// unbounded bill, so a negative one has to be refused rather than stored and
// later compared against.
func TestBudgetRejectsANegativeCap(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setBudget(context.Background(), interaction("g1", "", "settings"), -1)

	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.DailyBudgetUSD < 0 {
		t.Errorf("budget = %v, want a negative cap refused", cfg.DailyBudgetUSD)
	}
}

func TestBudgetStoresAValidCap(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setBudget(context.Background(), interaction("g1", "", "settings"), 5)

	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.DailyBudgetUSD != 5 {
		t.Errorf("budget = %v, want 5", cfg.DailyBudgetUSD)
	}
}

// Evidence retention 0 is a real setting, not an error: it is spec.MD's "log
// IDs, not content", and it costs the guild the ability to undo. Both ends of
// the range have to behave.
func TestEvidenceAcceptsZeroAndRefusesNegative(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setEvidence(context.Background(), interaction("g1", "", "settings"), 0)
	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.EvidenceHours != 0 {
		t.Errorf("evidence = %d, want 0 accepted as a deliberate choice", cfg.EvidenceHours)
	}

	p.setEvidence(context.Background(), interaction("g1", "", "settings"), -5)
	cfg, _ = store.Config(context.Background(), "g1")
	if cfg.EvidenceHours < 0 {
		t.Errorf("evidence = %d, want a negative window refused", cfg.EvidenceHours)
	}
}

func TestSanctionActionRoundTrips(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.setSanction(context.Background(), interaction("g1", "", "settings"), SanctionAction(string(SanctionJail)))
	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.SanctionAction != SanctionJail {
		t.Errorf("sanction action = %q, want jail", cfg.SanctionAction)
	}

	p.setSanction(context.Background(), interaction("g1", "", "settings"), SanctionAction("banish"))
	cfg, _ = store.Config(context.Background(), "g1")
	if cfg.SanctionAction == "banish" {
		t.Error("an invented sanction action was stored")
	}
}

// The exemption toggles are idempotent by design: adding a channel that is
// already exempt says so rather than writing a duplicate, which is what keeps
// the stored list from growing every time somebody re-runs the command.
func TestExemptTogglesAreIdempotent(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	// Two panels both drawn before channel 9 was exempt, both adding it.
	add := exemptClick(panelCfg+"exch", nil, "9")
	p.setExempt(context.Background(), add, true)
	p.setExempt(context.Background(), add, true)

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.ExemptChannelIDs) != 1 {
		t.Errorf("exempt channels = %v, want one entry after two identical adds", cfg.ExemptChannelIDs)
	}

	p.setExempt(context.Background(), exemptClick(panelCfg+"exch", []string{"9"}), true)
	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.ExemptChannelIDs) != 0 {
		t.Errorf("exempt channels = %v, want the channel removed", cfg.ExemptChannelIDs)
	}

	roleAdd := exemptClick(panelCfg+"exrole", nil, "19")
	p.setExempt(context.Background(), roleAdd, false)
	p.setExempt(context.Background(), roleAdd, false)
	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.ExemptRoleIDs) != 1 {
		t.Errorf("exempt roles = %v, want one entry after two identical adds", cfg.ExemptRoleIDs)
	}
}

// The read-only surfaces, on a guild with nothing configured and on one with
// everything. They are what an admin reads when trying to work out what the
// filter is doing, so failing on either shape fails at the worst moment.
func TestReadOnlySurfacesRender(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func() Config
	}{
		{"unconfigured", func() Config { return Config{GuildID: "g1", BucketActions: map[Bucket]Action{}} }},
		{"fully configured", func() Config {
			c := enforcingConfig()
			c.APIKeySealed = []byte("sealed-secret-value")
			c.ExemptChannelIDs = []string{"c1", "c2"}
			c.ExemptRoleIDs = []string{"r1"}
			c.FastModels = []string{"a/b"}
			c.SanctionOptInUserIDs = []string{"u1"}
			c.Calibration = []CalibrationExample{{Text: "in force", Bucket: BucketThreats}}
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.setConfig(tc.cfg())
			p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

			for _, pg := range aimodPages {
				p.panel(context.Background(), interaction("g1", "", "settings"), pg.key, "", "")
			}
			p.panel(context.Background(), interaction("g1", "", "settings"), "policy", string(BucketHateSpeech), "")
			p.handleStatus(context.Background(), interaction("g1", "", "status"))
			p.handleCalibrateShow(context.Background(), interaction("g1", "calibrate", "show"))
		})
	}
}

// Explaining a policy nobody has heard of is an ordinary typo, not a failure.
func TestPolicyExplainOnAnUnknownBucket(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.panel(context.Background(), interaction("g1", "", "settings"), "policy", "invented", "")
}

// /aimod undo has outcomes a moderator can genuinely hit, and they have to be
// told apart: no incident at all, one whose evidence window has passed so
// there is nothing left to restore, and one that actually reverses.
func TestUndoDistinguishesItsFailureModes(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	// Nothing recorded for this message.
	p.handleUndo(ctx, interaction("g1", "", "undo", strOpt("message_id", "m-unknown")))

	// Recorded, but with no stored text: the guild keeps no evidence.
	if _, err := store.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m-noevidence", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	p.handleUndo(ctx, interaction("g1", "", "undo", strOpt("message_id", "m-noevidence")))

	inc, err := store.IncidentByMessage(ctx, "g1", "m-noevidence")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	if inc.Undone {
		t.Error("an incident with nothing to restore was marked reversed anyway")
	}

	// Recorded with evidence, so this one actually reverses.
	if _, err := store.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m-real", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, Content: "the original words",
		CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	p.handleUndo(ctx, interaction("g1", "", "undo", strOpt("message_id", "m-real")))

	if inc, err = store.IncidentByMessage(ctx, "g1", "m-real"); err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	if !inc.Undone {
		t.Error("a reversible incident was not marked undone")
	}
}

// The config cache exists because this plugin reads config on the hot path,
// and every setter has to invalidate or a guild serves a stale answer for up
// to configTTL. configcache.go's own comment names that as the one thing to
// remember when adding a setter, so the two newest ones are asserted here.
func TestCachingStoreInvalidatesOnTheCalibrationSetters(t *testing.T) {
	ctx := context.Background()
	inner := newFakeStore()
	inner.setConfig(enforcingConfig())
	cached := NewCachingStore(inner)

	if _, err := cached.Config(ctx, "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if err := cached.SetCalibrationMode(ctx, "g1", CalibrationAuto); err != nil {
		t.Fatalf("SetCalibrationMode: %v", err)
	}
	got, err := cached.Config(ctx, "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got.CalibrationMode != CalibrationAuto {
		t.Error("SetCalibrationMode did not invalidate, so the guild serves a stale mode")
	}

	if err := cached.SetCalibration(ctx, "g1", []CalibrationExample{{Text: "x", Bucket: BucketThreats}}, nil, testNow); err != nil {
		t.Fatalf("SetCalibration: %v", err)
	}
	if got, err = cached.Config(ctx, "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if len(got.Calibration) != 1 {
		t.Error("SetCalibration did not invalidate, so the classifier keeps the old set")
	}
}

// exemptClick is an admin submitting one of the exemption pickers, drawn
// with drawn preselected, now holding picked.
func exemptClick(customID string, drawn []string, picked ...string) *core.Ix {
	pn := core.Panel{Parts: []discord.ContainerSubComponent{ui.Row(ui.ChannelSelectMany(customID, "", drawn, ui.MaxSelectValues, false))}}
	values := []any{}
	for _, v := range picked {
		values = append(values, v)
	}
	ix, _ := decodeIx("g1", 3, map[string]any{"custom_id": customID, "component_type": 8, "values": values},
		map[string]any{"id": "102", "channel_id": "101", "content": "", "flags": 64,
			"author": map[string]any{"id": "2", "username": "merlin"}, "components": pn.Layout()})
	return ix
}

// enabled reads the old command's boolean off an interaction built for it,
// so the opt-out switch tests can keep building their interactions as before.
func enabled(ix *core.Ix) bool {
	return ix.Args()["enabled"].Bool()
}
