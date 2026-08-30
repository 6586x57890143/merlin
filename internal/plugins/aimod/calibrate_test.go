package aimod

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The calibration loop is the one place a model's output feeds back into a
// prompt that deletes messages, so what it is allowed to say is the property
// worth pinning down. Everything here is about the boundary, not the wording.

func calibratingConfig() Config {
	cfg := enforcingConfig()
	cfg.BucketActions[BucketHateSpeech] = ActionRewrite
	return cfg
}

// calibratingConfigWithKey is the same config with a sealed API key, which is
// what checkBudget needs before it will report anything but "no key".
func calibratingConfigWithKey(t *testing.T, store *fakeStore) Config {
	t.Helper()
	stored, err := store.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	cfg := calibratingConfig()
	cfg.APIKeySealed = stored.APIKeySealed
	return cfg
}

// validateCalibration is what turns the paraphrase rule and the bucket
// restriction from prompt instructions into guarantees. Every case here is
// something a model has a real chance of returning.
func TestValidateCalibrationDropsWhatMustNotReachAPrompt(t *testing.T) {
	cfg := calibratingConfig()
	cfg.BucketActions[BucketSpam] = ActionOff

	good := CalibrationExample{
		Text: "this update is retarded", Bucket: BucketHateSpeech,
		ShouldAct: false, Note: "generic profanity about a thing",
	}

	in := []CalibrationExample{
		good,
		{Text: "", Bucket: BucketHateSpeech},
		{Text: strings.Repeat("x", maxCalibrationTextLen+1), Bucket: BucketHateSpeech},
		{Text: "something", Bucket: Bucket("invented_by_a_model")},
		{Text: "buy followers now", Bucket: BucketSpam},
		{Text: "shut up <@123456789>", Bucket: BucketHateSpeech},
		{Text: "go to https://example.com", Bucket: BucketHateSpeech},
		// An em dash. CI rejects these across the repo and this string is
		// rendered into a Discord embed by /aimod calibrate show. Written as
		// a code point for the same reason policy.go's own list is: spelling
		// it out would make this file fail the check it exists to assert.
		{Text: "a line with " + string(rune(0x2014)) + " in it", Bucket: BucketHateSpeech},
	}

	kept, problems := validateCalibration(cfg, in)
	if len(kept) != 1 {
		t.Fatalf("kept %d examples, want only the good one: %+v", len(kept), kept)
	}
	if kept[0].Text != good.Text || kept[0].Bucket != good.Bucket {
		t.Errorf("kept = %+v, want %+v", kept[0], good)
	}
	if len(problems) != len(in)-1 {
		t.Errorf("reported %d problems for %d bad entries", len(problems), len(in)-1)
	}
}

// A note that runs long costs the example nothing: it is commentary, and
// dropping a good example over its footnote would be the validator costing
// more than it saves.
func TestValidateCalibrationTrimsALongNoteRatherThanDroppingTheExample(t *testing.T) {
	kept, _ := validateCalibration(calibratingConfig(), []CalibrationExample{{
		Text: "this is retarded", Bucket: BucketHateSpeech,
		Note: strings.Repeat("y", maxCalibrationNoteLen+50),
	}})
	if len(kept) != 1 {
		t.Fatalf("kept %d, want the example retained with a trimmed note", len(kept))
	}
	if len([]rune(kept[0].Note)) != maxCalibrationNoteLen {
		t.Errorf("note length %d, want it trimmed to %d", len([]rune(kept[0].Note)), maxCalibrationNoteLen)
	}
}

func TestValidateCalibrationCapsTheSet(t *testing.T) {
	in := make([]CalibrationExample, maxCalibrationExamples+10)
	for i := range in {
		in[i] = CalibrationExample{Text: "an ordinary line", Bucket: BucketThreats}
	}
	kept, _ := validateCalibration(calibratingConfig(), in)
	if len(kept) != maxCalibrationExamples {
		t.Errorf("kept %d, want the cap of %d", len(kept), maxCalibrationExamples)
	}
}

// An uncalibrated guild's prompts must be byte-identical to what they were
// before this feature existed, or every guild pays for a feature it has not
// used.
func TestPromptsAreUnchangedWithoutCalibration(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	for name, prompt := range map[string]string{
		"fast": p.fastPrompt([]Bucket{BucketHateSpeech}, nil),
		"deep": p.deepPrompt(BucketHateSpeech, false, nil),
	} {
		if strings.Contains(prompt, "Calibration") {
			t.Errorf("%s prompt mentions calibration for a guild that has none", name)
		}
	}
}

// The deep pass is asking about exactly one policy, so it must carry exactly
// that policy's examples: room spent on the other nine is paid for on every
// escalation and answers a question nobody asked.
func TestDeepPromptCarriesOnlyItsOwnBucketsCalibration(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	cal := []CalibrationExample{
		{Text: "a hate speech example", Bucket: BucketHateSpeech},
		{Text: "a threats example", Bucket: BucketThreats},
	}

	prompt := p.deepPrompt(BucketHateSpeech, false, forBucket(cal, BucketHateSpeech))
	if !strings.Contains(prompt, "a hate speech example") {
		t.Error("the bucket's own calibration is missing")
	}
	if strings.Contains(prompt, "a threats example") {
		t.Error("another bucket's calibration reached a prompt that was not asking about it")
	}
}

// Rendering is done by this package, never by the model. The verdict wording
// around the example text is the containment boundary the whole feature rests
// on, so it is asserted rather than assumed.
func TestRenderCalibrationWritesTheVerdictItself(t *testing.T) {
	out := renderCalibration([]CalibrationExample{
		{Text: "this is retarded", Bucket: BucketHateSpeech, ShouldAct: false, Note: "generic profanity"},
		{Text: "a slur on its own", Bucket: BucketHateSpeech, ShouldAct: true},
	})
	for _, want := range []string{
		`- ordinary here, not hate_speech: "this is retarded" (generic profanity)`,
		`- act, hate_speech: "a slur on its own"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q, got:\n%s", want, out)
		}
	}
}

// The mode is the difference between a review that reports and one that acts,
// and getting it backwards would mean a suggest-mode guild silently running
// on calibration nobody approved.
func TestApplyCalibrationRespectsTheMode(t *testing.T) {
	proposed := []CalibrationExample{{Text: "a new example", Bucket: BucketHateSpeech}}
	existing := []CalibrationExample{{Text: "the one already in force", Bucket: BucketHateSpeech}}

	for _, tc := range []struct {
		mode        CalibrationMode
		wantActive  string
		wantPending int
	}{
		{CalibrationSuggest, "the one already in force", 1},
		{CalibrationAuto, "a new example", 0},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			store := newFakeStore()
			p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

			cfg := calibratingConfig()
			cfg.CalibrationMode = tc.mode
			cfg.Calibration = existing

			if err := p.applyCalibration(context.Background(), cfg, calibrationResult{Examples: proposed}); err != nil {
				t.Fatalf("applyCalibration: %v", err)
			}

			got, err := store.Config(context.Background(), cfg.GuildID)
			if err != nil {
				t.Fatalf("Config: %v", err)
			}
			if len(got.Calibration) != 1 || got.Calibration[0].Text != tc.wantActive {
				t.Errorf("active = %+v, want %q in force", got.Calibration, tc.wantActive)
			}
			if len(got.CalibrationPending) != tc.wantPending {
				t.Errorf("pending = %d, want %d", len(got.CalibrationPending), tc.wantPending)
			}
		})
	}
}

// The review job exists only where there is something to review, the same
// rule as rotation's sweep job. Both switches have to be on: mode off means
// nothing is classified, so there is no history to learn from and no prompt
// the result would ever reach.
func TestCalibrationJobRegistersOnlyWhenBothSwitchesAreOn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      Mode
		calibrate CalibrationMode
		want      bool
	}{
		{"both on", ModeEnforce, CalibrationSuggest, true},
		{"flag mode still reviews", ModeFlag, CalibrationAuto, true},
		{"plugin off", ModeOff, CalibrationSuggest, false},
		{"review off", ModeEnforce, CalibrationOff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched := &fakeScheduler{jobs: map[string]bool{}}
			p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
			p.sched = sched

			cfg := calibratingConfig()
			cfg.Mode, cfg.CalibrationMode = tc.mode, tc.calibrate
			p.reconcileCalibrationJob(context.Background(), cfg)

			if got := sched.jobs[calibrationJobKey(cfg.GuildID)]; got != tc.want {
				t.Errorf("job registered = %v, want %v", got, tc.want)
			}
			if tc.want && !sched.seeded[calibrationJobKey(cfg.GuildID)] {
				// Unseeded, a job the Scheduler has never seen is immediately
				// due, so switching this on would bill an unexpected review on
				// the next tick against a guild with no history yet.
				t.Error("job was registered without being seeded")
			}
		})
	}
}

// Turning either switch off has to remove the job, or a guild that opted out
// keeps paying for a weekly model call.
func TestCalibrationJobUnregisters(t *testing.T) {
	sched := &fakeScheduler{jobs: map[string]bool{}}
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.sched = sched

	cfg := calibratingConfig()
	cfg.Mode, cfg.CalibrationMode = ModeEnforce, CalibrationSuggest
	p.reconcileCalibrationJob(context.Background(), cfg)

	cfg.CalibrationMode = CalibrationOff
	p.reconcileCalibrationJob(context.Background(), cfg)

	if sched.jobs[calibrationJobKey(cfg.GuildID)] {
		t.Error("job survived the guild switching reviews off")
	}
}

// A quiet week is an ordinary outcome, not a failure. Paying a model to
// invent examples about a server it has not watched misbehave is how a
// calibration loop starts hallucinating norms.
func TestReviewRefusesWithNoHistory(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	if got := p.reviewInput(calibratingConfig(), nil); got != "" {
		t.Errorf("built a review input from no incidents: %q", got)
	}
}

// A reversal is a moderator saying the filter was wrong, and it is the only
// human-labelled data in the system. It has to reach the reviewer marked as
// such, or the most informative row in the table reads like every other one.
func TestReviewInputMarksModeratorReversals(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	out := p.reviewInput(calibratingConfig(), []Incident{
		{ChannelID: "c1", Bucket: BucketHateSpeech, Action: ActionRemove, Content: "wrongly removed", Undone: true},
		{ChannelID: "c1", Bucket: BucketThreats, Action: ActionRemove, Content: "correctly removed"},
	})
	if !strings.Contains(out, `"wrongly removed" [REVERSED BY A MODERATOR]`) {
		t.Errorf("a reversal was not marked for the reviewer:\n%s", out)
	}
	if strings.Contains(out, `"correctly removed" [REVERSED`) {
		t.Error("an incident nobody reversed was marked as reversed")
	}
}

// "Moderation heavy" is defined by where the filter has actually been busy,
// and the ordering has to be stable so two identical weeks do not read as
// different ones.
func TestBusiestChannelsRanksByIncidentCount(t *testing.T) {
	got := busiestChannels([]Incident{
		{ChannelID: "quiet"},
		{ChannelID: "busy"}, {ChannelID: "busy"}, {ChannelID: "busy"},
		{ChannelID: "middling"}, {ChannelID: "middling"},
	}, 2)
	want := []string{"busy", "middling"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("busiestChannels = %v, want %v", got, want)
	}
}

// fakeScheduler records registration rather than running anything.
type fakeScheduler struct {
	jobs   map[string]bool
	seeded map[string]bool
}

func (f *fakeScheduler) Register(key string, _ core.CronSpec, _ func(context.Context) error) error {
	f.jobs[key] = true
	return nil
}

func (f *fakeScheduler) Unregister(key string) error {
	delete(f.jobs, key)
	return nil
}

func (f *fakeScheduler) RunNow(context.Context, string) error { return nil }

func (f *fakeScheduler) Seed(_ context.Context, key string, _ time.Time) error {
	if f.seeded == nil {
		f.seeded = map[string]bool{}
	}
	f.seeded[key] = true
	return nil
}

func (f *fakeScheduler) NextDue(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// The review end to end, against a stub answer. This is the path the weekly
// job runs and the one /aimod calibrate run-now drives, so it covers the
// prompt build, the schema, the model call, the usage booking and the
// validation pass in one.
func TestReviewGuildEndToEnd(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	ops.history = []*discordgo.Message{
		{Content: "this update is retarded", Author: &discordgo.User{ID: "u1"}},
		{Content: "lol yeah", Author: &discordgo.User{ID: "u2"}},
	}
	client := &fakeClassifier{
		calibration: []string{`{"examples":[
			{"text":"this is retarded","bucket":"hate_speech","should_act":false,"note":"generic profanity"},
			{"text":"ping <@123>","bucket":"hate_speech","should_act":false,"note":"carries a mention"},
			{"text":"about a bucket nobody enforces","bucket":"spam","should_act":true,"note":""}
		],"findings":[{"summary":"acted on ordinary profanity twice","direction":"too_strict"}]}`},
		usage: Usage{Cost: 0.01, TotalTokens: 5000},
	}

	p := testPlugin(t, store, client, ops, &fakeAudit{})
	sealer, err := newSealer(testSecretKey)
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	p.sealer = sealer
	sealed, err := sealer.seal("sk-or-v1-test")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	cfg := calibratingConfig()
	cfg.APIKeySealed = sealed
	store.setConfig(cfg)

	if _, err := store.RecordIncident(context.Background(), Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketHateSpeech, Action: ActionRemove, Content: "this update is retarded",
		Reason: "slur", CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}

	res, err := p.reviewGuild(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reviewGuild: %v", err)
	}
	if len(res.Examples) != 1 || res.Examples[0].Text != "this is retarded" {
		t.Errorf("kept %+v, want only the clean example: the mention and the disabled bucket must be dropped", res.Examples)
	}
	if len(res.Findings) != 1 || res.Findings[0].Direction != "too_strict" {
		t.Errorf("findings = %+v, want the one the reviewer reported", res.Findings)
	}

	// Booked, because the call was billed whether or not the answer was
	// usable. A review that spent money without recording it is a hole in
	// the daily cap.
	spend, err := store.SpendToday(context.Background(), "g1", today(p.now()))
	if err != nil {
		t.Fatalf("SpendToday: %v", err)
	}
	if spend.SpentUSD == 0 {
		t.Error("the review's cost was not booked against the budget")
	}
}

// An unreadable answer is not a verdict. It must not become an empty
// calibration set that then replaces a good one.
func TestReviewGuildRejectsAnUnparseableAnswer(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{calibration: []string{"I cannot help with that."}}
	p := testPlugin(t, store, client, newFakeOps(), &fakeAudit{})
	sealer, _ := newSealer(testSecretKey)
	p.sealer = sealer
	sealed, _ := sealer.seal("sk-or-v1-test")

	cfg := calibratingConfig()
	cfg.APIKeySealed = sealed
	store.setConfig(cfg)
	if _, err := store.RecordIncident(context.Background(), Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketHateSpeech, Action: ActionRemove, CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}

	if _, err := p.reviewGuild(context.Background(), cfg); err == nil {
		t.Error("an unparseable review answer was reported as success")
	}
}

// Over the daily cap the review stops rather than overspending, and says so
// with its own sentinel so the weekly job can tell it from a real failure and
// not back itself off.
func TestReviewGuildStopsWhenTheBudgetIsSpent(t *testing.T) {
	store := newFakeStore()
	p := intakePlugin(t, store, &fakeClassifier{}, newFakeOps())
	cfg := calibratingConfigWithKey(t, store)
	cfg.DailyBudgetUSD = 0
	store.setConfig(cfg)
	_, err := p.reviewGuild(context.Background(), cfg)
	if !errors.Is(err, errCalibrationBudget) {
		t.Errorf("err = %v, want errCalibrationBudget", err)
	}
}

// The weekly job treats a quiet week and a spent budget as ordinary
// outcomes. Returning an error for either would count toward
// maxConsecutiveFailures and eventually fire the wedged-job alert against a
// guild that is behaving perfectly.
func TestCalibrationJobTreatsOrdinaryOutcomesAsSuccess(t *testing.T) {
	store := newFakeStore()
	p := intakePlugin(t, store, &fakeClassifier{}, newFakeOps())
	store.setConfig(calibratingConfigWithKey(t, store))

	if err := p.makeCalibrationJob("g1")(context.Background()); err != nil {
		t.Errorf("a week with no incidents was reported as a job failure: %v", err)
	}

	// And a guild switched off between the tick and the run is not a failure
	// either: reconcile unregisters on the next config change.
	off := calibratingConfigWithKey(t, store)
	off.CalibrationMode = CalibrationOff
	store.setConfig(off)
	if err := p.makeCalibrationJob("g1")(context.Background()); err != nil {
		t.Errorf("a guild that switched reviews off was reported as a job failure: %v", err)
	}
}

// SyncGuild is what cmd/bot calls on every GuildCreate. A nil scheduler (any
// build without one, and every other test in this package) must be a no-op
// rather than a panic on the gateway path.
func TestSyncGuildRegistersAndToleratesANilScheduler(t *testing.T) {
	store := newFakeStore()
	store.setConfig(calibratingConfig())

	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.SyncGuild(context.Background(), "g1") // nil scheduler, must not panic

	sched := &fakeScheduler{jobs: map[string]bool{}}
	p.sched = sched
	p.SyncGuild(context.Background(), "g1")
	if !sched.jobs[calibrationJobKey("g1")] {
		t.Error("SyncGuild did not register the review job for an enabled guild")
	}
}

// The reviewer is told which policies this guild actually enforces and what
// happens when one matches, so it cannot recommend examples for a bucket that
// is switched off. It is also shown the calibration already in force, so it
// revises rather than starting over every week.
func TestCalibrationPromptCarriesTheEnforcedPoliciesAndCurrentSet(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	cfg := calibratingConfig()
	cfg.BucketActions[BucketSpam] = ActionOff
	cfg.Calibration = []CalibrationExample{{Text: "already carried", Bucket: BucketHateSpeech}}

	prompt := p.calibrationPrompt(cfg)
	if !strings.Contains(prompt, string(BucketHateSpeech)) {
		t.Error("an enforced policy is missing from the reviewer's prompt")
	}
	if strings.Contains(prompt, string(BucketSpam)) {
		t.Error("a policy this guild switched off was offered to the reviewer")
	}
	if !strings.Contains(prompt, "already carried") {
		t.Error("the calibration in force was not shown, so the reviewer starts over every week")
	}
}

// The review is the largest call this package makes and the only one nobody
// is waiting on, so it has to route like every other call and it has to be
// allowed to take longer than one. Both were wrong at once in production: it
// sent OpenRouter's default stack to whichever gateway the guild was on, and
// the shared 20 second client timeout killed it before any model could
// answer, which read as an outage every half minute for ten hours.
func TestReviewGuildRoutesAndAllowsTimeToAnswer(t *testing.T) {
	store := newFakeStore()
	client := &fakeClassifier{calibration: []string{`{"examples":[],"findings":[]}`}}
	p := testPlugin(t, store, client, newFakeOps(), &fakeAudit{})
	sealer, err := newSealer(testSecretKey)
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	p.sealer = sealer
	sealed, err := sealer.seal("sk-orca-test")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	cfg := calibratingConfig()
	cfg.OrcaKeySealed = sealed
	store.setConfig(cfg)

	if _, err := store.RecordIncident(context.Background(), Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketHateSpeech, Action: ActionRemove, Content: "something",
		Reason: "slur", CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}

	if _, err := p.reviewGuild(context.Background(), cfg); err != nil {
		t.Fatalf("reviewGuild: %v", err)
	}

	req := client.lastCalibrationReq
	if req.spec != orcaRouter {
		t.Errorf("review went to %v, want the gateway the guild's key belongs to", req.spec)
	}
	if len(req.Models) == 0 || req.Models[0] != orcaRouter.deepModels[0] {
		t.Errorf("models = %v, want the routed gateway's deep stack", req.Models)
	}
	if req.timeout <= httpTimeout {
		t.Errorf("timeout = %v, want more than the scan path's %v: this prompt cannot be answered in that",
			req.timeout, httpTimeout)
	}
}

// The bucket a guild cannot switch off must not be relaxable by the weekly
// reviewer either. Everything about that reviewer points this way: it is
// asked to find over-strictness, its preamble tells it irony is the default
// reading, and on CalibrationAuto its answer applies with nobody reading it.
func TestCalibrationCannotStandDownChildSafety(t *testing.T) {
	cfg := calibratingConfig()
	kept, problems := validateCalibration(cfg, []CalibrationExample{
		{Text: "just a joke about minors", Bucket: BucketChildSafety, ShouldAct: false, Note: "reads as banter"},
		{Text: "an example that tightens it", Bucket: BucketChildSafety, ShouldAct: true, Note: "still a boast"},
	})
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly the stand-down example rejected", problems)
	}
	if len(kept) != 1 || !kept[0].ShouldAct {
		t.Fatalf("kept = %+v, want only the example that tightens the bucket", kept)
	}
}

// The reviewer and the classifier have to be reading the same rule. They are
// written in different places (a Go const and a YAML file) and nothing but
// this test stops one being edited without the other, which would produce the
// worst version of this mechanism: a reviewer that reports the filter as too
// strict for enforcing a policy it is required to enforce, week after week.
func TestCalibrationPromptCarriesTheSameChildSafetyRule(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	prompt := p.calibrationPrompt(calibratingConfig())

	if !strings.Contains(prompt, p.policies[BucketChildSafety].Short) {
		t.Error("the reviewer is not shown the child_safety short line the classifier judges by")
	}
	// The preamble tells it irony is the default reading of a heated line,
	// which is right everywhere except here. Without the carve-out it reads
	// a boast as banter and calls the removal a false positive.
	lower := strings.ToLower(prompt)
	if !strings.Contains(lower, "no humour exception") && !strings.Contains(lower, "grants no humour exception") {
		t.Error("the reviewer is not told that child_safety has no humour exception")
	}
	// Smarter, not more difficult: the stand-down example is refused, but
	// the observation has somewhere to go.
	if !strings.Contains(lower, "too_strict finding") {
		t.Error("the reviewer is not told where a genuine child_safety overreach should be reported instead")
	}
}
