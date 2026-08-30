package aimod

import (
	"context"
	"encoding/binary"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// Rung 1.5: a local guess at whether a message is worth a model call.
//
// The rung above it is the gate for everything below: nothing reaches the
// deep pass that the fast pass did not flag. So this rung is not a fourth
// opinion about policy, it is a cheap approximation of the fast pass's own
// output, and its only decision is whether to spend that call. It can skip
// or it can pass through. It can never flag, remove, rewrite or sanction,
// which is the same rule that keeps rung 2 off the delete path and is what
// makes a local model with no policy file safe to run here at all.
//
// The saving is real in two different currencies. A guild on OpenRouter pays
// per fast call, so skipping the obviously-fine majority is money. A guild on
// the default free OrcaRouter tier pays nothing per call but is limited by
// request rate, so the same skip is headroom, which is the resource that
// actually runs out there.
//
// ponytail: hashed n-grams and one logistic regression, about a hundred lines
// and no dependency. If this ever needs to be better, the upgrade path is a
// small local transformer behind the same two methods (Score and Learn), not
// a bigger version of this.

const (
	// triageBuckets is the hashed feature table size. A power of two so the
	// hash maps on with a mask rather than a modulo. 8192 float32s is 32KB
	// per guild, which is affordable per guild in memory and small enough to
	// write to Postgres whole.
	//
	// Collisions are frequent at this size and that is the hashing trick
	// working as intended, not a defect: a linear model absorbs them as
	// noise, and the alternative is holding a per-guild vocabulary, which is
	// a dictionary of the server's words in memory and on disk. Precisely
	// what this plugin's privacy posture rules out.
	triageBuckets = 1 << 13

	// triageWarmup is how many labelled examples a guild's model needs
	// before it is allowed to skip anything.
	//
	// Below it the model still learns and still scores, it simply never acts
	// on the score. That is what makes a fresh guild, a new deployment and a
	// restored backup all behave exactly as this plugin did before this rung
	// existed, rather than making their first few hundred messages the ones
	// an untrained model got to wave through.
	triageWarmup = 500

	// triageSkipThreshold is how sure the model has to be. Deliberately far
	// below a half: this is not "probably fine", it is "the model has
	// essentially never seen the fast pass flag anything like this".
	triageSkipThreshold = 0.02

	// triageSampleRate is the share of confidently-clean messages sent to the
	// fast pass anyway.
	//
	// This is not a hedge, it is what stops the rung eating itself. A model
	// that skips a region of its input stops receiving labels from that
	// region, so its picture of it freezes at whatever it believed on the day
	// it started skipping, and any later drift (a raid using new wording, a
	// community whose slang moved) is invisible precisely where it is being
	// trusted most. Sampling keeps labelled traffic flowing from the skipped
	// region forever, and is also the only honest measure of what the rung is
	// missing: see Stats.
	triageSampleRate = 0.05

	// triageLearnRate is plain SGD's step size. No schedule and no decay: the
	// target moves (a guild edits its bucket actions, its calibration set
	// changes what the fast pass flags), so a rate that decays to nothing
	// would leave the model pinned to a policy the guild has since changed.
	triageLearnRate = 0.08

	// triagePosWeight multiplies the step on a flagged example.
	//
	// Roughly one message in a hundred is flagged, and against imbalance like
	// that the loss is minimised by answering "clean" to everything. That
	// model would be right 99% of the time and would skip every violation on
	// the server, which is the exact failure this rung must not have. Costing
	// a missed positive far more than a wasted call is what buys precision on
	// the answer that matters.
	triagePosWeight = 12

	// triageWeightClip bounds every weight, standing in for a regularisation
	// term. One n-gram from one heated afternoon cannot come to dominate the
	// score, and it keeps the serialised model to a predictable range.
	triageWeightClip = 4.0

	// triageMaxFeatures bounds the work per message. A long paste is not
	// meaningfully better classified by its four thousandth n-gram, and this
	// runs on the gateway goroutine for every message in every guild.
	triageMaxFeatures = 512

	// triageSaveEvery is how many learned examples pass between saves.
	//
	// Proportional to activity rather than to the clock, which is what a
	// scheduler job would have been: a quiet guild writes almost never and a
	// busy one writes about as often as it has something new to say. Also
	// saved on Shutdown, so an orderly restart loses nothing.
	triageSaveEvery = 500
)

// triageModel is one guild's local classifier.
//
// Weights only. No examples, no text, no vocabulary: everything it knows
// arrived as a gradient step and cannot be read back out as anything a member
// wrote. That is the property that lets this learn continuously from live
// traffic without extending what the plugin retains by a single byte, and it
// is why training is online here rather than a nightly pass over stored
// messages. There is nothing stored to pass over, deliberately.
type triageModel struct {
	mu       sync.RWMutex
	w        []float32
	bias     float64
	examples int64
	// The counters behind /aimod status, and the reason shadow mode is worth
	// having. Not persisted: they describe this process's shift, and a total
	// carried across restarts would read as a claim about the model itself.
	//
	// considered and wouldSkip are incremented in every mode, which is the
	// whole point. In shadow the rung changes nothing, so these two are the
	// only evidence an admin has that turning it on would save anything.
	considered int64
	wouldSkip  int64
	// skipped is the subset actually not scanned, so it is zero unless the
	// mode is on. The gap between it and wouldSkip is the sampled share.
	skipped int64
	sampled int64
	// missed counts sampled messages the model would have skipped and the
	// fast pass then flagged. The rung's error rate, measured rather than
	// assumed, which is the whole reason sampling is not optional.
	missed int64
	// dirty is set by Learn and cleared by a save.
	dirty bool
}

func newTriageModel() *triageModel {
	return &triageModel{w: make([]float32, triageBuckets)}
}

// Score returns the model's estimate that the fast pass would flag this text.
func (m *triageModel) Score(feats []uint32) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sigmoid(m.rawLocked(feats))
}

func (m *triageModel) rawLocked(feats []uint32) float64 {
	z := m.bias
	for _, f := range feats {
		z += float64(m.w[f])
	}
	return z
}

// Ready reports whether the model has seen enough to be allowed to skip.
func (m *triageModel) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.examples >= triageWarmup
}

// Learn takes one labelled example: flagged is what the fast pass decided.
func (m *triageModel) Learn(feats []uint32, flagged bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	y := 0.0
	rate := triageLearnRate
	if flagged {
		y = 1
		rate *= triagePosWeight
	}
	// Gradient of the logistic loss with respect to the pre-sigmoid score.
	// Features are presence rather than counts, so each contributes exactly
	// this step and there is nothing to multiply by.
	g := sigmoid(m.rawLocked(feats)) - y
	step := float32(rate * g)
	for _, f := range feats {
		w := m.w[f] - step
		if w > triageWeightClip {
			w = triageWeightClip
		} else if w < -triageWeightClip {
			w = -triageWeightClip
		}
		m.w[f] = w
	}
	m.bias -= rate * g
	m.examples++
	m.dirty = true
}

// TriageStats is what /aimod status reports about this rung.
type TriageStats struct {
	Examples   int64
	Considered int64
	WouldSkip  int64
	Skipped    int64
	Sampled    int64
	Missed     int64
	Ready      bool
}

func (m *triageModel) Stats() TriageStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return TriageStats{
		Examples: m.examples, Considered: m.considered, WouldSkip: m.wouldSkip,
		Skipped: m.skipped, Sampled: m.sampled, Missed: m.missed,
		Ready: m.examples >= triageWarmup,
	}
}

func sigmoid(z float64) float64 {
	// Clamped before the exponential: a saturated score is the normal state
	// for a confident linear model, and math.Exp of a large positive number
	// is +Inf, which turns the gradient into a NaN that then poisons every
	// weight it touches.
	if z > 30 {
		return 1
	}
	if z < -30 {
		return 0
	}
	return 1 / (1 + math.Exp(-z))
}

// triageFeatures turns text into hashed feature indices.
//
// Character n-grams rather than words, because the input is Discord chat: it
// is misspelled, it is deliberately obfuscated, it runs words together, and a
// word-level model has to see every spelling of a slur separately while a
// character model generalises across them for free. Word unigrams are added
// alongside because they carry the ordinary topical signal that says a
// message is a normal conversation, which is the half this rung actually
// acts on.
func triageFeatures(text string) []uint32 {
	norm := normalizeForTriage(text)
	if norm == "" {
		return nil
	}

	seen := make(map[uint32]struct{}, 128)
	out := make([]uint32, 0, 128)
	add := func(kind byte, s string) bool {
		h := hashFeature(kind, s)
		if _, dup := seen[h]; dup {
			return true
		}
		seen[h] = struct{}{}
		out = append(out, h)
		return len(out) < triageMaxFeatures
	}

	for _, word := range strings.Fields(norm) {
		if !add('w', word) {
			return out
		}
	}
	// Padded so the start and end of a message are themselves signal: a
	// message that opens on a slur and one that mentions it in passing are
	// different messages.
	padded := " " + norm + " "
	r := []rune(padded)
	const n = 4
	for i := 0; i+n <= len(r); i++ {
		if !add('c', string(r[i:i+n])) {
			return out
		}
	}
	return out
}

// normalizeForTriage lowercases, collapses whitespace and drops the
// punctuation that carries no signal, so "K Y S !!!" and "kys" land on
// mostly the same features.
func normalizeForTriage(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := true
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			space = false
		case r == '\'' || r == '@' || r == '/' || r == '.':
			// Kept: they are load bearing in handles, links and the
			// letter-substitution spellings this rung exists to generalise
			// over.
			b.WriteRune(r)
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// hashFeature is FNV-1a over a one byte kind tag and the token, masked into
// the table. The tag keeps a word and a character n-gram that happen to be
// the same string on separate weights.
func hashFeature(kind byte, s string) uint32 {
	const offset32 = 2166136261
	const prime32 = 16777619
	h := uint32(offset32)
	h = (h ^ uint32(kind)) * prime32
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * prime32
	}
	return h & (triageBuckets - 1)
}

// neverSkipPattern forces a full scan regardless of what the model thinks.
//
// The child safety bucket is the one place in this package where the cheap
// answer is not allowed to be the final one. Every other force here pushes
// toward leniency: the system preamble tells the model that dark humour is
// ordinary on this server, the calibration reviewer hunts for over-strictness,
// and this rung's whole purpose is to decide that most messages are fine.
// Discord's policy has no such exception, so the model's opinion is overridden
// rather than trusted, and the override is one-way: matching here only ever
// causes a message to be scanned normally, exactly as it would have been
// before this rung existed.
//
// Deliberately over-inclusive, and cheap to be so. A false positive costs one
// fast-pass call that would have happened anyway a week ago; a false negative
// is the one outcome this bucket exists to prevent. It is not a detector and
// must never be read as one: nothing acts on a match, it only declines to
// save money.
var neverSkipPattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`\b(minors?|underage|under[- ]?age|jail ?bait|pre[- ]?teens?|pre[- ]?pubescent)\b`,
	`\b(lolis?|shotas?|c\.?p\.?|csa[m]?)\b`,
	`\b(child|children|kids?|toddlers?|infants?)\b`,
	`\b(1[0-7]|[1-9])\s*(y[/.]?o|years?[- ]old|yr?s?[- ]old)\b`,
	`\b(grade|year)\s*(school|[1-9]|1[0-2])\b`,
	`\b(school\s*(girl|boy|kid)|middle\s*school|elementary)\b`,
}, "|"))

// mustScan reports that this text may never be skipped on a model's guess.
func mustScan(text string) bool { return neverSkipPattern.MatchString(text) }

// encodeTriageModel serialises the weights for storage: little-endian float32
// bits, bias appended as a float64. A fixed-width blob rather than JSON,
// because it is 8192 numbers and the only reader is decodeTriageModel.
func encodeTriageModel(m *triageModel) ([]byte, int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	buf := make([]byte, 4*len(m.w)+8)
	for i, w := range m.w {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(w))
	}
	binary.LittleEndian.PutUint64(buf[4*len(m.w):], math.Float64bits(m.bias))
	return buf, m.examples
}

// decodeTriageModel restores a stored model, or reports that it cannot.
//
// A blob of the wrong length is refused rather than padded or truncated. It
// means the table size changed between releases, and reinterpreting the old
// weights against a new hash space would give a model that is confidently
// wrong about everything, which is far worse here than one that starts again:
// starting again just means a warmup during which nothing is skipped.
func decodeTriageModel(raw []byte, examples int64) (*triageModel, bool) {
	if len(raw) != 4*triageBuckets+8 {
		return nil, false
	}
	m := newTriageModel()
	for i := range m.w {
		m.w[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	m.bias = math.Float64frombits(binary.LittleEndian.Uint64(raw[4*triageBuckets:]))
	m.examples = examples
	return m, true
}

// TriageMode is how much a guild lets this rung do.
//
// Three values rather than a bool, and the middle one is the point. shadow
// scores every message, learns from every verdict and reports what it would
// have saved, while still sending everything to the fast pass. An admin can
// then read a real number off /aimod status before trusting it with their
// server's coverage, which is the same shape as calibration_mode's
// suggest-before-auto and exists for the same reason: this changes what gets
// looked at, so a guild should watch it work once rather than discover it.
type TriageMode string

const (
	TriageOff    TriageMode = "off"
	TriageShadow TriageMode = "shadow"
	TriageOn     TriageMode = "on"
)

func (m TriageMode) Valid() bool {
	switch m {
	case TriageOff, TriageShadow, TriageOn:
		return true
	}
	return false
}

// triageDecision is what the rung concluded about one message.
type triageDecision struct {
	// skip is the only field that changes what happens: true means no model
	// call. It is never set in shadow mode.
	skip bool
	// wouldSkip is what the model thought, regardless of mode. Shadow mode
	// runs on this, and it is also what makes a sampled message countable as
	// a near miss.
	wouldSkip bool
	// sampled marks a message the model would have skipped that is being
	// scanned anyway, to keep labels flowing and to measure the miss rate.
	sampled bool
}

// triageFor returns a guild's model, loading it from storage on first use.
//
// A failed load yields a fresh model rather than an error: an untrained model
// skips nothing, so the worst outcome of an unreadable row is that this guild
// scans everything, which is what it did before this rung existed.
func (p *Plugin) triageFor(ctx context.Context, guildID string) *triageModel {
	p.triageMu.Lock()
	if p.triage == nil {
		// New fills this in, but the plugin is also built field-wise in
		// tests, where a nil map would panic on the first message rather than
		// on the path under test. Same guard, and the same reason, as
		// reconcileFundingJob's.
		p.triage = map[string]*triageModel{}
	}
	if m, ok := p.triage[guildID]; ok {
		p.triageMu.Unlock()
		return m
	}
	p.triageMu.Unlock()

	m := newTriageModel()
	if raw, examples, err := p.store.TriageModel(ctx, guildID); err == nil && len(raw) > 0 {
		if restored, ok := decodeTriageModel(raw, examples); ok {
			m = restored
		} else {
			p.log.Warn("aimod: stored triage model does not fit this build, starting fresh",
				"guild", guildID, "bytes", len(raw))
		}
	} else if err != nil {
		p.log.Error("aimod: load triage model", "guild", guildID, "err", err)
	}

	// Re-checked under the lock: two messages in the same guild can race here
	// and the loser must not replace a model the winner has already begun
	// teaching.
	p.triageMu.Lock()
	defer p.triageMu.Unlock()
	if existing, ok := p.triage[guildID]; ok {
		return existing
	}
	p.triage[guildID] = m
	return m
}

// triageDecide asks the local model whether this message needs a model call.
//
// Fails toward scanning at every step: an off mode, an unready model, a
// vetoed text, a score above the threshold and the sampled share all end up
// at the same place, which is the behaviour this plugin had before the rung
// existed.
func (p *Plugin) triageDecide(ctx context.Context, cfg Config, text string) triageDecision {
	if cfg.TriageMode == TriageOff {
		return triageDecision{}
	}
	// The veto is checked before the model is even consulted, so no amount of
	// confidence can route around it. See neverSkipPattern.
	if mustScan(text) {
		return triageDecision{}
	}
	feats := triageFeatures(text)
	if len(feats) == 0 {
		return triageDecision{}
	}
	m := p.triageFor(ctx, guildIDOf(cfg))

	m.mu.Lock()
	m.considered++
	ready := m.examples >= triageWarmup
	m.mu.Unlock()

	if !ready || m.Score(feats) >= triageSkipThreshold {
		return triageDecision{}
	}

	d := triageDecision{wouldSkip: true}
	// Sampling is decided over the messages the model would have skipped, so
	// the rate is a share of that population rather than of all traffic. That
	// is what makes the miss count a rate this rung is answerable for rather
	// than a statistic about the server.
	sample := p.sampleTriage()

	m.mu.Lock()
	m.wouldSkip++
	switch {
	case sample:
		d.sampled = true
		m.sampled++
	case cfg.TriageMode == TriageOn:
		d.skip = true
		m.skipped++
	}
	m.mu.Unlock()
	return d
}

// sampleTriage decides whether a would-be skip is scanned anyway.
//
// Nil-safe for the same reason the map above is: a field-wise plugin in a test
// has no sampler, and the safe default there is not to sample, so a test that
// says "this should be skipped" is not defeated by an unseeded coin.
func (p *Plugin) sampleTriage() bool {
	if p.triageSample == nil {
		return false
	}
	return p.triageSample()
}

// triageLearn records what the fast pass decided about one message.
//
// The label is the fast pass's own answer, not a policy judgement of its own,
// because this rung is an approximation of that gate and nothing else. It is
// also the only training input: no stored text is ever read back, and none is
// written, so continuous learning here costs nothing in retention.
func (p *Plugin) triageLearn(ctx context.Context, cfg Config, text string, flagged bool) {
	if cfg.TriageMode == TriageOff {
		return
	}
	feats := triageFeatures(text)
	if len(feats) == 0 {
		return
	}
	m := p.triageFor(ctx, guildIDOf(cfg))
	m.Learn(feats, flagged)

	m.mu.Lock()
	save := m.dirty && m.examples%triageSaveEvery == 0
	m.mu.Unlock()
	if save {
		p.spawn(func(bg context.Context) { p.saveTriage(bg, guildIDOf(cfg)) })
	}
}

// triageMiss records that a sampled message the model wanted to skip was
// flagged by the fast pass after all.
func (p *Plugin) triageMiss(ctx context.Context, cfg Config) {
	m := p.triageFor(ctx, guildIDOf(cfg))
	m.mu.Lock()
	m.missed++
	m.mu.Unlock()
}

// saveTriage persists one guild's weights.
func (p *Plugin) saveTriage(ctx context.Context, guildID string) {
	p.triageMu.Lock()
	m, ok := p.triage[guildID]
	p.triageMu.Unlock()
	if !ok {
		return
	}
	raw, examples := encodeTriageModel(m)
	if err := p.store.SaveTriageModel(ctx, guildID, raw, examples); err != nil {
		p.log.Error("aimod: save triage model", "guild", guildID, "err", err)
		return
	}
	m.mu.Lock()
	m.dirty = false
	m.mu.Unlock()
}

// saveAllTriage flushes every loaded model, called from Shutdown so an
// orderly restart resumes rather than warming up again.
func (p *Plugin) saveAllTriage(ctx context.Context) {
	p.triageMu.Lock()
	ids := make([]string, 0, len(p.triage))
	for id, m := range p.triage {
		m.mu.RLock()
		dirty := m.dirty
		m.mu.RUnlock()
		if dirty {
			ids = append(ids, id)
		}
	}
	p.triageMu.Unlock()

	for _, id := range ids {
		p.saveTriage(ctx, id)
	}
}

// guildIDOf is the one place that reads the guild off a Config, so the hot
// path reads the same field the store keyed the row by.
func guildIDOf(cfg Config) string { return cfg.GuildID }
