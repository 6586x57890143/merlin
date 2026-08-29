package aimod

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
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
// What makes them testable is a session whose HTTP transport answers every
// Discord call with an empty success (see discordStub). The handler runs to
// completion, its replies go nowhere, and the assertions are about what it
// did rather than what it said. No network, no Discord, no fixture server.

// discordStub answers every Discord REST call with an empty success, without
// touching the network.
//
// Success rather than failure, and that is load bearing: a handler that does
// real work starts with core.DeferResponse and returns early if it fails, so
// a transport that errored would make every deferring handler (undo, models
// show, calibrate run-now) bail on line one and test nothing at all.
type discordStub struct{}

func (discordStub) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

func testSession(t *testing.T) *discordgo.Session {
	t.Helper()
	// discordgo.New rather than a zero Session: the REST path locks a rate
	// limit bucket before it builds a request, and a nil Ratelimiter panics.
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	s.Client = &http.Client{Transport: discordStub{}}
	return s
}

// interaction builds a command interaction for one leaf of /aimod.
//
// path is the subcommand group and subcommand ("calibrate", "mode"), matching
// how CommandRouter.Handle names them, so a test reads the same way the
// registration does.
func interaction(guildID, group, sub string, args ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	leaf := &discordgo.ApplicationCommandInteractionDataOption{
		Name:    sub,
		Type:    discordgo.ApplicationCommandOptionSubCommand,
		Options: args,
	}
	opts := []*discordgo.ApplicationCommandInteractionDataOption{leaf}
	if group != "" {
		opts = []*discordgo.ApplicationCommandInteractionDataOption{{
			Name:    group,
			Type:    discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{leaf},
		}}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:      "i1",
		Token:   "tok",
		GuildID: guildID,
		Type:    discordgo.InteractionApplicationCommand,
		Member: &discordgo.Member{
			User:  &discordgo.User{ID: "actor"},
			Roles: []string{},
		},
		Data: discordgo.ApplicationCommandInteractionData{
			Name:    "aimod",
			Options: opts,
		},
	}}
}

func strOpt(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value,
	}
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
	s := testSession(t)

	p.handleSetMode(context.Background(), s, interaction("g1", "configure", "mode", strOpt("mode", "enforce")))

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

	p.handleSetMode(context.Background(), testSession(t), interaction("g1", "configure", "mode", strOpt("mode", "flag")))

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

	p.handleSetMode(context.Background(), testSession(t), interaction("g1", "configure", "mode", strOpt("mode", "sideways")))

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

	p.handlePolicySet(context.Background(), testSession(t), interaction("g1", "policy", "set",
		strOpt("policy", string(BucketChildSafety)), strOpt("action", string(ActionOff))))

	cfg, _ := store.Config(context.Background(), "g1")
	if got := EffectiveAction(cfg.BucketActions, BucketChildSafety); got != ActionRemove {
		t.Errorf("child_safety = %q, want remove regardless of what was stored", got)
	}
}

func TestPolicySetStoresAValidAction(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.handlePolicySet(context.Background(), testSession(t), interaction("g1", "policy", "set",
		strOpt("policy", string(BucketHateSpeech)), strOpt("action", string(ActionRewrite))))

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

	p.handleCalibrateMode(context.Background(), testSession(t),
		interaction("g1", "calibrate", "mode", strOpt("mode", string(CalibrationAuto))))

	got, _ := store.Config(context.Background(), "g1")
	if got.CalibrationMode != CalibrationAuto {
		t.Errorf("calibration mode = %q, want auto", got.CalibrationMode)
	}
	if !sched.jobs[calibrationJobKey("g1")] {
		t.Error("the weekly job was not registered, so the setting does nothing")
	}

	p.handleCalibrateMode(context.Background(), testSession(t),
		interaction("g1", "calibrate", "mode", strOpt("mode", string(CalibrationOff))))
	if sched.jobs[calibrationJobKey("g1")] {
		t.Error("switching reviews off left the job registered")
	}
}

func TestCalibrateModeRejectsAnUnknownValue(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.handleCalibrateMode(context.Background(), testSession(t),
		interaction("g1", "calibrate", "mode", strOpt("mode", "whenever")))

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
	p.handleCalibrateApply(context.Background(), testSession(t), interaction("g1", "calibrate", "apply"))

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
	p.handleCalibrateApply(context.Background(), testSession(t), interaction("g1", "calibrate", "apply"))

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
	p.handleCalibrateClear(context.Background(), testSession(t), interaction("g1", "calibrate", "clear"))

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
			p.handleCalibrateShow(context.Background(), testSession(t), interaction("g1", "calibrate", "show"))
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
	p.handleCalibrateRunNow(context.Background(), testSession(t), interaction("g1", "calibrate", "run-now"))

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

	p.handleCalibrateRunNow(context.Background(), testSession(t), interaction("g1", "calibrate", "run-now"))

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

	on := interaction("g1", "", "moderate-me", &discordgo.ApplicationCommandInteractionDataOption{
		Name: "enabled", Type: discordgo.ApplicationCommandOptionBoolean, Value: true,
	})
	p.handleModerateMe(context.Background(), testSession(t), on)

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.SanctionOptInUserIDs) != 1 || cfg.SanctionOptInUserIDs[0] != "actor" {
		t.Errorf("opt-in list = %v, want the caller added", cfg.SanctionOptInUserIDs)
	}

	off := interaction("g1", "", "moderate-me", &discordgo.ApplicationCommandInteractionDataOption{
		Name: "enabled", Type: discordgo.ApplicationCommandOptionBoolean, Value: false,
	})
	p.handleModerateMe(context.Background(), testSession(t), off)

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
		&discordgo.ApplicationCommandInteractionDataOption{
			Name: "user", Type: discordgo.ApplicationCommandOptionUser, Value: "victim",
		},
		&discordgo.ApplicationCommandInteractionDataOption{
			Name: "enabled", Type: discordgo.ApplicationCommandOptionBoolean, Value: true,
		})
	p.handleModerateUser(context.Background(), testSession(t), i)

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.SanctionOptInUserIDs) != 0 {
		t.Errorf("a non-bootstrap caller opted somebody else in: %v", cfg.SanctionOptInUserIDs)
	}

	p.privilege = fakePrivilege{bootstrapID: "actor"}
	p.handleModerateUser(context.Background(), testSession(t), i)
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

	p.handleWhy(context.Background(), testSession(t), interaction("g1", "", "why", strOpt("message_id", "m-unknown")))
}

// /aimod status is the command an admin runs when they think something is
// wrong, so it has to render on a guild where everything is unset.
func TestStatusRendersOnAnUnconfiguredGuild(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.handleStatus(context.Background(), testSession(t), interaction("g1", "", "status"))
}

func numOpt(name string, v float64) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionNumber, Value: v,
	}
}

func intOpt(name string, v float64) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: v,
	}
}

func boolOpt(name string, v bool) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionBoolean, Value: v,
	}
}

// The daily cap is the only thing between a misconfigured guild and an
// unbounded bill, so a negative one has to be refused rather than stored and
// later compared against.
func TestBudgetRejectsANegativeCap(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.handleSetBudget(context.Background(), testSession(t), interaction("g1", "configure", "budget", numOpt("usd", -1)))

	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.DailyBudgetUSD < 0 {
		t.Errorf("budget = %v, want a negative cap refused", cfg.DailyBudgetUSD)
	}
}

func TestBudgetStoresAValidCap(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	p.handleSetBudget(context.Background(), testSession(t), interaction("g1", "configure", "budget", numOpt("usd", 5)))

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
	s := testSession(t)

	p.handleSetEvidence(context.Background(), s, interaction("g1", "configure", "evidence", intOpt("hours", 0)))
	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.EvidenceHours != 0 {
		t.Errorf("evidence = %d, want 0 accepted as a deliberate choice", cfg.EvidenceHours)
	}

	p.handleSetEvidence(context.Background(), s, interaction("g1", "configure", "evidence", intOpt("hours", -5)))
	cfg, _ = store.Config(context.Background(), "g1")
	if cfg.EvidenceHours < 0 {
		t.Errorf("evidence = %d, want a negative window refused", cfg.EvidenceHours)
	}
}

func TestSanctionActionRoundTrips(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	s := testSession(t)

	p.handleSetSanction(context.Background(), s, interaction("g1", "configure", "sanctions",
		strOpt("action", string(SanctionJail))))
	cfg, _ := store.Config(context.Background(), "g1")
	if cfg.SanctionAction != SanctionJail {
		t.Errorf("sanction action = %q, want jail", cfg.SanctionAction)
	}

	p.handleSetSanction(context.Background(), s, interaction("g1", "configure", "sanctions",
		strOpt("action", "banish")))
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
	s := testSession(t)

	add := interaction("g1", "configure", "exempt-channel", strOpt("channel", "c9"), boolOpt("exempt", true))
	p.handleExemptChannel(context.Background(), s, add)
	p.handleExemptChannel(context.Background(), s, add)

	cfg, _ := store.Config(context.Background(), "g1")
	if len(cfg.ExemptChannelIDs) != 1 {
		t.Errorf("exempt channels = %v, want one entry after two identical adds", cfg.ExemptChannelIDs)
	}

	p.handleExemptChannel(context.Background(), s,
		interaction("g1", "configure", "exempt-channel", strOpt("channel", "c9"), boolOpt("exempt", false)))
	cfg, _ = store.Config(context.Background(), "g1")
	if len(cfg.ExemptChannelIDs) != 0 {
		t.Errorf("exempt channels = %v, want the channel removed", cfg.ExemptChannelIDs)
	}

	roleAdd := interaction("g1", "configure", "exempt-role", strOpt("role", "r9"), boolOpt("exempt", true))
	p.handleExemptRole(context.Background(), s, roleAdd)
	p.handleExemptRole(context.Background(), s, roleAdd)
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
			s := testSession(t)

			p.handleConfigureShow(context.Background(), s, interaction("g1", "configure", "show"))
			p.handlePolicyList(context.Background(), s, interaction("g1", "policy", "list"))
			p.handlePolicyExplain(context.Background(), s,
				interaction("g1", "policy", "explain", strOpt("policy", string(BucketHateSpeech))))
			p.handleStatus(context.Background(), s, interaction("g1", "", "status"))
			p.handleCalibrateShow(context.Background(), s, interaction("g1", "calibrate", "show"))
		})
	}
}

// Explaining a policy nobody has heard of is an ordinary typo, not a failure.
func TestPolicyExplainOnAnUnknownBucket(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.handlePolicyExplain(context.Background(), testSession(t),
		interaction("g1", "policy", "explain", strOpt("policy", "invented")))
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
	s := testSession(t)

	// Nothing recorded for this message.
	p.handleUndo(ctx, s, interaction("g1", "", "undo", strOpt("message_id", "m-unknown")))

	// Recorded, but with no stored text: the guild keeps no evidence.
	if _, err := store.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m-noevidence", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	p.handleUndo(ctx, s, interaction("g1", "", "undo", strOpt("message_id", "m-noevidence")))

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
	p.handleUndo(ctx, s, interaction("g1", "", "undo", strOpt("message_id", "m-real")))

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
