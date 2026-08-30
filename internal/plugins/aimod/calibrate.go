package aimod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The weekly self-review.
//
// The rest of this package draws one fixed line: minScanLen, the thresholds,
// and the policy catalogue are compiled in and identical on every server. The
// line they are trying to draw is not. "this is retarded" is ordinary
// profanity on a blunt server and a slur on a quiet one, and irony, sarcasm
// and in-group banter move it further than any wording in a YAML file can
// chase. So once a week this reads the parts of a guild's chat where the
// filter has actually been busy, asks a model where the filter was too strict
// and too lenient against how that community really talks, and turns the
// answer into a small set of labelled examples the classifier prompts carry.
//
// What keeps this from being a bot that rewrites its own rules: the learned
// artifact is not text. It is []CalibrationExample, four typed fields, and
// every sentence that reaches a prompt is written by renderCalibration below,
// not by the model. A calibration entry can say "text like this, in this
// bucket, should or should not be acted on" and there is no field in which it
// can say anything else. systemPreamble, policy/*.yaml, defaultActions,
// deepThreshold and actThreshold are all out of its reach, and stay validated
// at startup by LoadPolicies exactly as before.

const (
	// reviewWindow is how much history one run reads. A week, matching the
	// cadence, so consecutive runs neither overlap much nor leave a gap.
	reviewWindow = 7 * 24 * time.Hour

	// maxReviewIncidents bounds the incident query. Well above what a normal
	// guild produces in a week, and a hard stop for one being raided.
	maxReviewIncidents = 400

	// maxReviewChannels is how many channels the transcript is drawn from:
	// the busiest few by incident count, which is what "moderation heavy"
	// means operationally. Reading every channel would cost more and say
	// less, since the quiet ones are quiet precisely because nothing in them
	// is near the line.
	maxReviewChannels = 3

	// reviewMessagesPerChannel is the transcript depth per channel.
	reviewMessagesPerChannel = 60

	// maxReviewChars caps the whole transcript. This, not the message counts
	// above, is what actually bounds the bill: it is the one number to lower
	// if a run costs more than a guild wants to pay.
	maxReviewChars = 24000

	// calibrateMaxTokens is the answer ceiling. Larger than the deep pass
	// because the answer is a list of examples plus findings, and the same
	// reasoning-token headroom argument applies (see deepMaxTokens): a model
	// that thinks its way through the ceiling returns nothing at all.
	calibrateMaxTokens = 6000

	// calibrateTimeout is how long one review may take.
	//
	// Minutes rather than the scan path's seconds, because nothing about
	// this call resembles the scan path: it sends the largest prompt this
	// package builds, asks for the longest answer, and nobody is standing in
	// front of it. Under httpTimeout it could not finish on any guild with a
	// real week of history behind it, and the failure it produced was
	// indistinguishable from an outage. Comfortably inside the Scheduler's
	// own jobTimeout even if Chat spends its one retry.
	calibrateTimeout = 3 * time.Minute
)

// Bounds on what one run may produce. Validation limits rather than
// formatting preferences: all of this is rendered into a prompt that is paid
// for on every batch, and into a Discord embed that is rejected whole if it
// runs long.
const (
	maxCalibrationExamples = 24
	maxCalibrationTextLen  = 160
	maxCalibrationNoteLen  = 160
	// maxFastCalibration is how many examples the fast prompt carries. Fewer
	// than the deep prompt gets, because that prompt is amortized across
	// every batch in every guild forever while the deep prompt is paid for
	// roughly one message in a hundred. The same short-line/full-file split
	// the policy catalogue already makes, for the same reason.
	maxFastCalibration = 6
)

// finding is one thing the review noticed, for the humans. Deliberately not
// fed back into any prompt: the examples are the machine-readable half, and
// this is the half a moderator reads to decide whether to trust them.
type finding struct {
	Summary   string `json:"summary"`
	Direction string `json:"direction"` // too_strict | too_lenient
}

// calibrationResult is one review's answer.
type calibrationResult struct {
	Examples []CalibrationExample `json:"examples"`
	Findings []finding            `json:"findings"`
}

// calibrationPreamble frames the reviewer.
//
// Deliberately more explicit than systemPreamble about the community it is
// judging for. The classifier prompts say "not a civility filter" and leave
// it there; this one has to say what the alternative standard actually is,
// because its whole job is to notice where the compiled-in wording and this
// server's real norms have come apart. A reviewer left to its own instincts
// recommends the filter be stricter every single time, which would make this
// mechanism a slow ratchet toward exactly the thing the plugin exists to
// prevent.
const calibrationPreamble = `You are auditing an automated moderation filter that runs on a Discord server, and reporting where it is getting things wrong.

The server is a blunt, high-free-speech internet community. Judge by what that kind of community treats as ordinary, not by what is polite:

- Insults, profanity, mockery, hostile argument, dark humour and offensive opinions are ordinary here. They are not violations and the filter must not act on them.
- Irony, sarcasm, hyperbole, trash talk and in-group banter are the DEFAULT reading of a heated line in a fast channel, not the exception. "I am going to kill you" after a lost match is a figure of speech. Read the surrounding messages before concluding that anything is sincere.
- Words with slur origins used as generic profanity about an idea, an object or one person's behaviour are ordinary here and must be left alone. "this is retarded", "you are retarded" and "that patch is gay" are NOT violations on this server. Treat any incident where the filter acted on one of those as a clear false positive.
- What genuinely does breach Discord's Community Guidelines should be acted on almost every time: slurs aimed at people for a protected characteristic, credible threats against a real person, doxxing, sexual content involving minors, promotion of violent extremism, malware and phishing.

The two mistakes are not equal, but both are real. Acting on ordinary speech drives away the members the server was built for; missing a genuine breach is what gets the whole server reported off the platform. Report both directions honestly.

You will be given recent messages from the channels where this filter has been most active, and a list of what the filter decided. Some decisions are marked REVERSED BY A MODERATOR. Those are the only human-labelled data here: weight them above your own reading, and work out what the filter misread.

Return calibration examples the filter should carry from now on. These rules are checked, and an example that breaks one is discarded:

- Write each example as a SHORT PARAPHRASE, or an invented stand-in with the same shape as what you saw. Never quote a real message verbatim. Never include a username, a mention, a channel reference, an ID or a link.
- Use only the policy names you were given.
- Prefer examples that correct something you actually watched go wrong over examples of things that already work.
- Fewer, sharper examples beat a long list. Return an empty list if the filter looks correctly calibrated.`

func calibrationSchema() *responseFormat {
	return &responseFormat{
		Type: "json_schema",
		JSONSchema: jsonSchema{
			Name:   "calibration",
			Strict: true,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"examples": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text":       map[string]any{"type": "string"},
								"bucket":     map[string]any{"type": "string"},
								"should_act": map[string]any{"type": "boolean"},
								"note":       map[string]any{"type": "string"},
							},
							"required":             []string{"text", "bucket", "should_act", "note"},
							"additionalProperties": false,
						},
					},
					"findings": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"summary":   map[string]any{"type": "string"},
								"direction": map[string]any{"type": "string", "enum": []string{"too_strict", "too_lenient"}},
							},
							"required":             []string{"summary", "direction"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"examples", "findings"},
				"additionalProperties": false,
			},
		},
	}
}

// errNothingToReview reports that a guild gave the reviewer nothing to work
// from. Not a failure: a week with no incidents is a week with nothing to
// correct, and paying a model to invent examples about a server it has not
// watched misbehave is how a calibration loop starts hallucinating norms.
var errNothingToReview = errors.New("aimod: no moderation history in the review window")

// errCalibrationBudget reports that the guild is over its daily cap. A
// review is the most expensive single call this plugin makes, so it is the
// first thing that should stop, and it stopping must never look like it ran
// and found nothing.
var errCalibrationBudget = errors.New("aimod: daily budget spent, calibration review skipped")

// reviewGuild runs one review and returns the validated examples plus the
// findings. It changes nothing; applyCalibration decides what to do with the
// answer.
func (p *Plugin) reviewGuild(ctx context.Context, cfg Config) (calibrationResult, error) {
	state, err := p.checkBudget(ctx, cfg)
	if err != nil {
		return calibrationResult{}, fmt.Errorf("aimod: calibration budget check: %w", err)
	}
	if state.Exhausted {
		// The same degradation as the scan path: over the cap this stops
		// rather than overspending, and says so rather than failing quietly.
		return calibrationResult{}, errCalibrationBudget
	}

	since := p.now().Add(-reviewWindow)
	incidents, err := p.store.IncidentsSince(ctx, cfg.GuildID, since, maxReviewIncidents)
	if err != nil {
		return calibrationResult{}, fmt.Errorf("aimod: calibration read incidents: %w", err)
	}

	user := p.reviewInput(cfg, incidents)
	if user == "" {
		return calibrationResult{}, errNothingToReview
	}

	out, usage, err := p.client.Chat(ctx, state.APIKey, chatRequest{
		spec:     state.Spec,
		timeout:  calibrateTimeout,
		Models:   modelsOr(cfg.DeepModels, state.Spec.deepModels),
		Provider: strictProvider(),
		Messages: []chatMessage{
			{Role: "system", Content: p.calibrationPrompt(cfg)},
			{Role: "user", Content: user},
		},
		ResponseFormat: calibrationSchema(),
		MaxTokens:      calibrateMaxTokens,
	})
	// Booked before the error is handled, matching classify: a call that
	// returned usage was billed whether or not its body parsed.
	if usage.Cost > 0 || usage.TotalTokens > 0 {
		p.recordUsage(ctx, cfg.GuildID, usage, true)
	}
	if err != nil {
		return calibrationResult{}, fmt.Errorf("aimod: calibration review: %w", err)
	}

	var res calibrationResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return calibrationResult{}, fmt.Errorf("aimod: calibration returned unparseable JSON: %w", err)
	}

	kept, problems := validateCalibration(cfg, res.Examples)
	if len(problems) > 0 {
		// Logged rather than surfaced: a discarded example is the validator
		// doing its job, not something an admin has to act on.
		p.log.Warn("aimod: discarded calibration examples",
			"guild", cfg.GuildID, "kept", len(kept), "discarded", len(problems))
	}
	res.Examples = kept
	return res, nil
}

// reviewInput builds the user message: what the filter did, then what the
// channels it did it in actually look like.
//
// Returns "" when there is nothing worth paying to review. See
// errNothingToReview.
func (p *Plugin) reviewInput(cfg Config, incidents []Incident) string {
	if len(incidents) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("What the filter decided in the last week:\n")
	for _, inc := range incidents {
		mark := ""
		if inc.Undone {
			mark = " [REVERSED BY A MODERATOR]"
		}
		text := inc.Content
		if text == "" {
			// A guild on evidence 0, or a row already past its retention
			// window. The decision still carries information with the text
			// gone, which is why these are included rather than skipped, but
			// the review is weaker and /aimod calibrate show says so.
			text = "(text not retained)"
		}
		fmt.Fprintf(&b, "- %s/%s (confidence %.0f%%): %q%s. Filter's reason: %s\n",
			inc.Bucket, inc.Action, inc.Confidence*100, sanitizeForPrompt(text), mark, inc.Reason)
	}

	b.WriteString("\nRecent conversation in the channels where it has been most active:\n")
	budget := maxReviewChars
	for _, channelID := range busiestChannels(incidents, maxReviewChannels) {
		msgs, err := p.ops(cfg.GuildID).ChannelMessages(channelID, reviewMessagesPerChannel, "", "", "")
		if err != nil {
			// The same policy as recentContext: an unreadable channel makes
			// the review weaker, never fatal.
			continue
		}
		fmt.Fprintf(&b, "\n-- channel %s --\n", channelID)
		labels := newSpeakerLabels()
		// Discord returns newest-first; reverse so the reviewer reads the
		// conversation in the order it happened.
		for i := len(msgs) - 1; i >= 0 && budget > 0; i-- {
			if msgs[i] == nil || msgs[i].Content == "" || msgs[i].Author == nil {
				continue
			}
			line := labels.of(msgs[i].Author.ID) + ": " + sanitizeForPrompt(msgs[i].Content) + "\n"
			if len(line) > budget {
				break
			}
			budget -= len(line)
			b.WriteString(line)
		}
	}
	return b.String()
}

// busiestChannels ranks a guild's channels by how many incidents happened in
// them, which is this feature's working definition of "moderation heavy".
func busiestChannels(incidents []Incident, n int) []string {
	counts := map[string]int{}
	for _, inc := range incidents {
		counts[inc.ChannelID]++
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	// Count descending, then ID, so a tie does not reorder between runs and
	// make two identical weeks look like different ones.
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > n {
		ids = ids[:n]
	}
	return ids
}

// calibrationPrompt is the reviewer's system message: the framing, the policy
// names it is allowed to use, and whatever calibration is already in force so
// it revises rather than starting over.
func (p *Plugin) calibrationPrompt(cfg Config) string {
	var b strings.Builder
	b.WriteString(calibrationPreamble)
	b.WriteString("\n\nThe policies this server enforces, and what happens when one is matched:\n")
	for _, bucket := range enforcedBuckets(cfg) {
		pol, ok := p.policies[bucket]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", bucket, EffectiveAction(cfg.BucketActions, bucket), pol.Short)
	}
	if len(cfg.Calibration) > 0 {
		b.WriteString("\nCalibration the filter is already carrying. Keep what still holds, drop what this history shows was wrong, and add what is missing:\n")
		b.WriteString(renderCalibration(cfg.Calibration))
	}
	return b.String()
}

// validateCalibration drops what must not reach a prompt and returns the
// survivors plus a description of everything discarded.
//
// The runtime analogue of validatePolicy, with one deliberate difference:
// that one refuses to boot, this one drops entries and carries on. A policy
// file is something a human wrote and can fix; this is a model's answer
// arriving on a schedule at four in the morning, and failing the whole run
// because one entry of twelve was malformed would mean a guild silently never
// calibrates at all.
//
// This function is what makes the typed-example design a guarantee rather
// than an intention: the paraphrase rule and the bucket restriction are asked
// for in the prompt, and enforced here.
func validateCalibration(cfg Config, in []CalibrationExample) (kept []CalibrationExample, problems []string) {
	for _, ex := range in {
		text := strings.TrimSpace(ex.Text)
		note := strings.TrimSpace(ex.Note)
		if text == "" {
			problems = append(problems, "example with no text")
			continue
		}
		if len([]rune(text)) > maxCalibrationTextLen {
			problems = append(problems, "example text over the length cap")
			continue
		}
		if len([]rune(note)) > maxCalibrationNoteLen {
			// The note is trimmed rather than the entry dropped: it is
			// commentary, and losing a good example over its footnote would
			// be the validator costing more than it saves.
			note = string([]rune(note)[:maxCalibrationNoteLen])
		}
		if !known(ex.Bucket) {
			problems = append(problems, "example for unknown policy "+string(ex.Bucket))
			continue
		}
		if EffectiveAction(cfg.BucketActions, ex.Bucket) == ActionOff {
			// Not wrong, just pointless: a disabled bucket is sent to
			// neither pass, so an example about it is prompt weight buying
			// nothing.
			problems = append(problems, "example for disabled policy "+string(ex.Bucket))
			continue
		}
		if identifying(text) || identifying(note) {
			problems = append(problems, "example carries a mention, ID or link")
			continue
		}
		if hasGeneratedPunctuation(text) || hasGeneratedPunctuation(note) {
			// These strings are rendered into Discord embeds by
			// /aimod calibrate show, and this repo holds what it displays to
			// the same punctuation bar CI enforces on its own source.
			problems = append(problems, "example carries generated punctuation")
			continue
		}
		kept = append(kept, CalibrationExample{
			Text: text, Bucket: ex.Bucket, ShouldAct: ex.ShouldAct, Note: note,
		})
		if len(kept) == maxCalibrationExamples {
			break
		}
	}
	return kept, problems
}

// identifying reports whether s carries something pointing at a real person,
// channel or destination. The prompt asks for paraphrases; this is the check
// that makes the asking a guarantee, and it matters because this column is
// not covered by the evidence-retention window.
func identifying(s string) bool {
	lower := strings.ToLower(s)
	for _, bad := range []string{"<@", "<#", "http://", "https://", "discord.gg/", "www."} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

func hasGeneratedPunctuation(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return slices.Contains(generatedPunctuation, r)
	})
}

// renderCalibration turns stored examples into the lines a prompt carries.
// This function, not the model, writes every word around the example text:
// that is the containment boundary the whole feature rests on.
func renderCalibration(examples []CalibrationExample) string {
	var b strings.Builder
	for _, ex := range examples {
		verdict := "ordinary here, not " + string(ex.Bucket)
		if ex.ShouldAct {
			verdict = "act, " + string(ex.Bucket)
		}
		fmt.Fprintf(&b, "- %s: %q", verdict, ex.Text)
		if ex.Note != "" {
			fmt.Fprintf(&b, " (%s)", ex.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// calibrationBlock is what the classifier prompts append. Empty for an
// uncalibrated guild, which is what keeps those prompts byte-identical to
// what they were before this feature existed.
func calibrationBlock(examples []CalibrationExample) string {
	if len(examples) == 0 {
		return ""
	}
	return "\nCalibration from this server's own moderation history. It describes how this specific community actually talks; where one of these applies, follow it over your own instinct:\n" +
		renderCalibration(examples)
}

// forBucket narrows the set to one policy, which is exactly what the deep
// pass is asking about. Free and exact: no room in that prompt is spent on
// nine policies it was not handed.
func forBucket(examples []CalibrationExample, bucket Bucket) []CalibrationExample {
	var out []CalibrationExample
	for _, ex := range examples {
		if ex.Bucket == bucket {
			out = append(out, ex)
		}
	}
	return out
}

// firstN bounds what the fast prompt carries.
func firstN(examples []CalibrationExample, n int) []CalibrationExample {
	if len(examples) > n {
		return examples[:n]
	}
	return examples
}

// applyCalibration stores a finished review according to the guild's mode,
// and audits what it did.
//
// The split is the whole point of having a mode: in suggest the active set is
// not touched, so a run cannot change what the filter does merely by having
// happened, and an admin reads the proposal first.
func (p *Plugin) applyCalibration(ctx context.Context, cfg Config, res calibrationResult) error {
	active, pending := cfg.Calibration, res.Examples
	action := "aimod.calibration_proposed"
	if cfg.CalibrationMode == CalibrationAuto {
		active, pending = res.Examples, nil
		action = "aimod.calibration_applied"
	}
	if err := p.store.SetCalibration(ctx, cfg.GuildID, active, pending, p.now()); err != nil {
		return fmt.Errorf("aimod: store calibration: %w", err)
	}
	if err := p.auditWriter.Record(ctx, cfg.GuildID, core.ActorSystem, action, "",
		core.TruncateEmbedField(summariseReview(cfg.CalibrationMode, res))); err != nil {
		// Audit-failure policy: the review already happened and is already
		// stored, so a failed embed post must not undo it.
		p.log.Error("aimod: audit calibration", "guild", cfg.GuildID, "err", err)
	}
	return nil
}

// summariseReview is the sentence a moderator reads in the audit channel.
func summariseReview(mode CalibrationMode, res calibrationResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Weekly review produced %d calibration example(s).", len(res.Examples))
	if mode == CalibrationAuto {
		b.WriteString(" Applied automatically.")
	} else {
		b.WriteString(" Run `/aimod calibrate show` to read them, `/aimod calibrate apply` to put them in force.")
	}
	for _, f := range res.Findings {
		direction := "too lenient"
		if f.Direction == "too_strict" {
			direction = "too strict"
		}
		fmt.Fprintf(&b, "\n- %s: %s", direction, f.Summary)
	}
	return b.String()
}

// calibrationSchedule is when the weekly review runs.
//
// CalendarSchedule rather than IntervalSchedule, so a restart or a missed
// tick never walks the review off its hour the way re-deriving from elapsed
// time would. Early Sunday UTC: after a full week of traffic and outside the
// hours anybody is likely to be watching a busy server.
var calibrationSchedule = func() core.CronSpec {
	sunday := time.Sunday
	return core.CronSpec{Schedule: core.CalendarSchedule{Weekday: &sunday, HourUTC: 4}}
}

// calibrationJobKey names a guild's review job.
func calibrationJobKey(guildID string) string { return guildID + ":aimod-calibrate" }

// SyncGuild reconciles the review job for one guild. Called from the
// GuildCreate handler in cmd/bot/main.go, and from the two handlers that can
// change the answer.
//
// Unlike rotation's SyncGuild this does not depend on internal/settings
// having loaded, because this plugin reads its own config; a guild whose
// settings refresh failed still gets its review job.
func (p *Plugin) SyncGuild(ctx context.Context, guildID string) {
	if p.sched == nil {
		return
	}
	// The tip jar reads its own table, so it reconciles before the config
	// read below and independently of whether that read succeeds. The two
	// jobs have nothing to do with each other, and skipping both because one
	// query failed is exactly how a guild ends up with a job that never gets
	// registered for the life of the process.
	if f, err := p.store.Funding(ctx, guildID); err != nil {
		p.log.Error("aimod: load funding for sync", "guild", guildID, "err", err)
	} else {
		p.reconcileFundingJob(guildID, f.Configured())
	}

	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		p.log.Error("aimod: load config for calibration sync", "guild", guildID, "err", err)
		return
	}
	p.reconcileCalibrationJob(ctx, cfg)
}

// reconcileCalibrationJob registers the review only where there is something
// to review, the same rule as rotation.reconcileSweepJob: a job armed in a
// guild that has not opted in is a scheduled model call nobody asked for.
//
// Both switches have to be on. Mode off means nothing is being classified, so
// there is no history to learn from and no prompt the result would reach.
func (p *Plugin) reconcileCalibrationJob(ctx context.Context, cfg Config) {
	if p.sched == nil {
		return
	}
	p.calibrateMu.Lock()
	defer p.calibrateMu.Unlock()
	if p.calibrateRegistered == nil {
		// New fills this in, but the plugin is also constructed field-wise in
		// tests, and a nil map here panics on the first registration rather
		// than on the path anybody was testing.
		p.calibrateRegistered = map[string]bool{}
	}

	key := calibrationJobKey(cfg.GuildID)
	needed := cfg.Mode != ModeOff && cfg.CalibrationMode != CalibrationOff
	registered := p.calibrateRegistered[cfg.GuildID]

	switch {
	case needed && !registered:
		if err := p.sched.Register(key, calibrationSchedule(), p.makeCalibrationJob(cfg.GuildID)); err != nil {
			p.log.Error("aimod: register calibration job", "guild", cfg.GuildID, "err", err)
			return
		}
		p.calibrateRegistered[cfg.GuildID] = true
		// Seeded so the first real fire is a full week out. A job the
		// Scheduler has never seen is otherwise immediately due, and an admin
		// who has just switched this on would get an unexpected model bill on
		// the next tick, against a guild that has no history to learn from
		// yet. /aimod calibrate run-now is the deliberate immediate one.
		if err := p.sched.Seed(ctx, key, p.now()); err != nil {
			p.log.Error("aimod: seed calibration job", "guild", cfg.GuildID, "err", err)
		}
	case !needed && registered:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("aimod: unregister calibration job", "guild", cfg.GuildID, "err", err)
			return
		}
		delete(p.calibrateRegistered, cfg.GuildID)
	}
}

// makeCalibrationJob is the function the Scheduler calls once a week.
//
// It re-reads config rather than closing over it, because a week is long
// enough for every setting it depends on to have changed since registration.
func (p *Plugin) makeCalibrationJob(guildID string) func(context.Context) error {
	return func(ctx context.Context) error {
		cfg, err := p.store.Config(ctx, guildID)
		if err != nil {
			return fmt.Errorf("aimod: calibration load config: %w", err)
		}
		if cfg.Mode == ModeOff || cfg.CalibrationMode == CalibrationOff {
			// Switched off between the tick and now. Not an error: reconcile
			// will unregister on the next config change, and failing here
			// would count toward maxConsecutiveFailures and alert on a job
			// that is behaving correctly.
			return nil
		}
		res, err := p.reviewGuild(ctx, cfg)
		switch {
		case errors.Is(err, errNothingToReview), errors.Is(err, errCalibrationBudget):
			// Both are ordinary outcomes, not failures. Returning an error
			// for a quiet week would back the job off and eventually fire the
			// wedged-job alert for a guild that is simply peaceful.
			p.log.Info("aimod: calibration review skipped", "guild", guildID, "reason", err)
			return nil
		case err != nil:
			return err
		}
		return p.applyCalibration(ctx, cfg, res)
	}
}

func calibrationModeChoices() []*discordgo.ApplicationCommandOptionChoice {
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(CalibrationModes))
	for _, m := range CalibrationModes {
		out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: string(m), Value: string(m)})
	}
	return out
}

func (p *Plugin) handleCalibrateShow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Weekly review:** %s\n", cfg.CalibrationMode)
	if cfg.CalibrationRanAt.IsZero() {
		b.WriteString("**Last run:** never\n")
	} else {
		fmt.Fprintf(&b, "**Last run:** <t:%d:R>\n", cfg.CalibrationRanAt.Unix())
	}
	if cfg.EvidenceHours == 0 {
		// Worth saying here rather than only in /aimod configure show: the
		// review reads stored message text, so a guild keeping none gets a
		// materially weaker one and has no way to find that out.
		b.WriteString("\n> Evidence retention is off, so the review can see what the filter decided but not what it decided about. Set `/aimod configure evidence` above 0 for a sharper one.\n")
	}

	b.WriteString("\n**In force**\n")
	if len(cfg.Calibration) == 0 {
		b.WriteString("_Nothing. The filter is running on the built-in policies alone._\n")
	} else {
		b.WriteString(renderCalibration(cfg.Calibration))
	}

	if len(cfg.CalibrationPending) > 0 {
		b.WriteString("\n**Proposed, not yet in force**\n")
		b.WriteString(renderCalibration(cfg.CalibrationPending))
		b.WriteString("\nRun `/aimod calibrate apply` to put these in force, or `/aimod calibrate clear` to drop them.\n")
	}

	// The body interpolates an unbounded example list, and Discord rejects an
	// over-limit description outright rather than trimming it, so without
	// this the command would fail exactly when it had most to show.
	core.RespondInfo(s, i, "Filter calibration", core.TruncateEmbedDescription(b.String()))
}

func (p *Plugin) handleCalibrateRunNow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	// A transcript fetch plus the largest model call this plugin makes, so
	// nowhere near Discord's 3 second window.
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer calibrate run-now", "err", err)
		return
	}
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}
	if cfg.Mode == ModeOff {
		_ = core.FollowUpErr(s, i, "Nothing to review",
			errors.New("this plugin is off, so there is no moderation history to learn from. Set `/aimod configure mode` first"))
		return
	}

	res, err := p.reviewGuild(ctx, cfg)
	switch {
	case errors.Is(err, errNothingToReview):
		_ = core.FollowUpErr(s, i, "Nothing to review",
			errors.New("nothing has been actioned or flagged in the last week, so there is nothing to calibrate against yet"))
		return
	case errors.Is(err, errCalibrationBudget):
		_ = core.FollowUpErr(s, i, "Daily budget spent", err)
		return
	case err != nil:
		_ = core.FollowUpErr(s, i, "The review failed", err)
		return
	}
	if len(res.Examples) == 0 {
		_ = core.FollowUpOK(s, i, "Nothing to change",
			"The review read the last week and found nothing worth correcting. The filter's current calibration stands.")
		return
	}
	// run-now honours the guild's mode rather than applying regardless. An
	// admin running this on suggest is asking for the weekly review early,
	// not asking to skip the approval step they configured.
	if err := p.applyCalibration(ctx, cfg, res); err != nil {
		_ = core.FollowUpErr(s, i, "Could not store the result", err)
		return
	}
	if cfg.CalibrationMode == CalibrationAuto {
		_ = core.FollowUpOK(s, i, "Calibration updated",
			fmt.Sprintf("%d example(s) are now in force. `/aimod calibrate show` to read them.", len(res.Examples)))
		return
	}
	_ = core.FollowUpOK(s, i, "Calibration proposed",
		fmt.Sprintf("%d example(s) are waiting. `/aimod calibrate show` to read them, `/aimod calibrate apply` to put them in force.", len(res.Examples)))
}

func (p *Plugin) handleCalibrateApply(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	if len(cfg.CalibrationPending) == 0 {
		core.RespondWarn(s, i, "Nothing proposed",
			"There is no pending calibration to apply. `/aimod calibrate run-now` produces one.")
		return
	}
	// Re-validated against live config rather than trusted from the column.
	// A proposal can sit for a week, and a bucket switched off in the
	// meantime must not come back through a stored example.
	kept, _ := validateCalibration(cfg, cfg.CalibrationPending)
	if err := p.store.SetCalibration(ctx, i.GuildID, kept, nil, time.Time{}); err != nil {
		core.RespondErr(s, i, "Failed to apply it", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.calibration_applied",
		fmt.Sprintf("%d in force", len(cfg.Calibration)), fmt.Sprintf("%d in force", len(kept)))
	core.RespondOK(s, i, "Calibration applied",
		fmt.Sprintf("%d example(s) are now in force and will be carried by every scan from here on.", len(kept)))
}

func (p *Plugin) handleCalibrateClear(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	if err := p.store.SetCalibration(ctx, i.GuildID, nil, nil, time.Time{}); err != nil {
		core.RespondErr(s, i, "Failed to clear it", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.calibration_cleared",
		fmt.Sprintf("%d in force", len(cfg.Calibration)), "0 in force")
	core.RespondOK(s, i, "Calibration cleared",
		"The filter is back to the built-in policies alone. The weekly review still runs unless you also set `/aimod calibrate mode off`.")
}

func (p *Plugin) handleCalibrateMode(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	mode := CalibrationMode(core.LeafArgs(i)["mode"].Value.(string))
	if !mode.valid() {
		core.RespondErr(s, i, "Unknown mode", fmt.Errorf("%q is not a review mode", mode))
		return
	}
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	if err := p.store.SetCalibrationMode(ctx, i.GuildID, mode); err != nil {
		core.RespondErr(s, i, "Failed to set the mode", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.calibration_mode_set", string(cfg.CalibrationMode), string(mode))

	// Registering or unregistering the job is this setting's whole effect, so
	// it has to happen here: this plugin has no EventConfigChanged
	// subscription, because it owns its own config.
	cfg.CalibrationMode = mode
	p.reconcileCalibrationJob(ctx, cfg)

	switch mode {
	case CalibrationAuto:
		core.RespondWarn(s, i, "Reviewing weekly, applying automatically",
			"Every Sunday the filter will review the week's moderation and change its own calibration without asking. "+
				"`/aimod calibrate show` to read what it is carrying, `/aimod calibrate clear` to undo it.")
	case CalibrationSuggest:
		core.RespondOK(s, i, "Reviewing weekly, proposing only",
			"Every Sunday the filter will review the week's moderation and post what it would change to the audit log. "+
				"Nothing takes effect until you run `/aimod calibrate apply`.")
	default:
		core.RespondOK(s, i, "Reviews off",
			"The filter will run on the built-in policies plus whatever calibration is already in force. "+
				"`/aimod calibrate clear` also drops that.")
	}
}
