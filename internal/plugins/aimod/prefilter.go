package aimod

import (
	"hash/fnv"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The free rungs of the ladder. Everything in this file runs in-process,
// costs nothing, and decides the overwhelming majority of messages, which is
// the only reason the paid rungs stay affordable on a busy server.

// minScanLen is the length below which a message is not worth a model call.
//
// It was 12, and that was the single largest hole in this filter: a slur
// typed on its own is six characters, "kys" is three, and every one of them
// returned skipShort before anything looked at it. The version of this
// comment that set 12 already named "kys" as the case that argued for zero,
// and settled on 12 for cost. It was answering the wrong question: rung 1 is
// a fixed table of credential and link shapes, so "the short strings that
// genuinely matter are caught by pattern" was only ever true of the strings
// somebody had already thought to write a pattern for. Slurs are not a
// closed set, and a per-word regex table is an evasion treadmill.
//
// What changed since is that dedupeCache remembers verdicts rather than
// sightings, which is what makes 3 affordable. Short chatter ("lol", "ok",
// "same", "lmao") is the most-repeated text on any server, so the first copy
// inside dedupeWindow costs one slot in a batch that was going out anyway
// and every later copy is skipDuplicate for free. The floor now only
// suppresses single emoji and one-word acknowledgements.
//
// Fast-tier volume does rise, and honestly: this trades money for coverage.
// checkBudget is what bounds the money, and over the cap the plugin degrades
// to rungs 0-1 rather than overspending.
const minScanLen = 3

// dedupeWindow is how long a verdict on an exact piece of text stands.
//
// Half an hour rather than the five minutes it started at, and the reason it
// could be widened is that a repeat is no longer a message that goes
// unexamined. A cached verdict is now applied to every copy (see
// dedupeCache), so a longer window means more messages moderated for the
// same money rather than more messages let through.
//
// Bounded rather than indefinite because identical text genuinely can mean
// different things in different conversations, and because a verdict that
// outlived a policy change would keep enforcing a rule a guild had already
// turned off.
const dedupeWindow = 30 * time.Minute

// dedupeMax bounds the cache so a long-running process cannot grow it
// without limit. Well above what any real guild needs inside the window.
const dedupeMax = 4096

// skipReason names why a message never reached a model. Returned rather than
// a bare bool so /aimod status can be honest about what is being skipped and
// the tests can assert on the reason rather than the outcome.
type skipReason string

const (
	skipNone      skipReason = ""
	skipBot       skipReason = "author is a bot"
	skipWebhook   skipReason = "webhook message"
	skipMode      skipReason = "plugin mode is off"
	skipChannel   skipReason = "channel is exempt"
	skipRole      skipReason = "author holds an exempt role"
	skipEmpty     skipReason = "no text content"
	skipShort     skipReason = "shorter than the scan floor"
	skipDuplicate skipReason = "identical text seen recently"
)

// dedupeCache remembers recently-decided message text by hash.
//
// A hash, not the text: this cache lives for the process lifetime and would
// otherwise be a copy of the server's chat sitting in memory, which is
// exactly what this plugin's privacy property rules out. A collision costs
// one skipped scan inside a five minute window, which is a fair trade for
// not holding the content.
// A verdict, not just a sighting. This is what lets the filter moderate a
// flood without paying for it: the first copy of a message is classified,
// and every later copy inside the window is acted on from the remembered
// answer, with no model call and nothing drawn from the member's scan
// ceiling.
//
// It replaces two earlier designs, both wrong in instructive ways. The first
// recorded every message as seen and skipped the repeats outright, so a
// member who posted the same slur four times had one flagged and three
// silently dropped. The second only remembered clean text, which fixed that
// but made every repeat pay full price again, so a raid could exhaust a
// ceiling with fifty copies of one line. Remembering the verdict gets both:
// every copy is acted on, and only the first one costs anything.
type dedupeEntry struct {
	at time.Time
	// verdict is nil when the text was scanned and had nothing against it.
	verdict *cachedVerdict
}

// cachedVerdict is everything enforce needs to act on a repeat without
// asking a model again.
type cachedVerdict struct {
	bucket Bucket
	action Action
	deep   deepVerdict
}

type dedupeCache struct {
	mu   sync.Mutex
	seen map[uint64]dedupeEntry
}

func newDedupeCache() *dedupeCache {
	return &dedupeCache{seen: make(map[uint64]dedupeEntry)}
}

// dedupeKey hashes guild, an optional author scope, and normalized text.
//
// A hash, not the text: this cache lives for the process lifetime and would
// otherwise be a copy of the server's chat sitting in memory, which is
// exactly what this plugin's privacy property rules out.
//
// authorID is empty for everything except a deep-pass clear. See markCleanFor
// for why that one entry is narrower than the rest.
func dedupeKey(guildID, authorID, content string) uint64 {
	h := fnv.New64a()
	// Guild-scoped so two servers running the same copypasta do not mask
	// each other, and because a verdict is only meaningful against one
	// guild's policy anyway.
	_, _ = h.Write([]byte(guildID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(authorID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(content))))
	return h.Sum64()
}

// seenClean reports whether this exact text was scanned inside the window
// and came back with nothing against it.
//
// Only clean text is ever remembered; see markClean. The check and the
// record are separate calls for that reason, where they used to be one, and
// that split is the whole fix: the combined version recorded every message
// as it went past, so the second and later copies of a message that was
// about to be flagged were dropped before anything looked at them. On a
// server where somebody repeated a slur four times, the first was flagged
// and the rest produced no record at all, which reads from the outside as
// the filter having stopped working. Repetition is aggravating, not
// exculpatory, and it must never be the thing that buys silence.
// A guild-wide clear is checked first, then one recorded for this author
// alone: see markCleanFor.
func (c *dedupeCache) seenClean(guildID, authorID, content string, now time.Time) bool {
	if e, ok := c.lookup(guildID, content, now); ok && e.verdict == nil {
		return true
	}
	e, ok := c.lookupKey(dedupeKey(guildID, authorID, content), now)
	return ok && e.verdict == nil
}

// lookup returns a live guild-wide entry for this text, if there is one.
func (c *dedupeCache) lookup(guildID, content string, now time.Time) (dedupeEntry, bool) {
	return c.lookupKey(dedupeKey(guildID, "", content), now)
}

func (c *dedupeCache) lookupKey(key uint64, now time.Time) (dedupeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.seen[key]
	if !ok || now.Sub(e.at) >= dedupeWindow {
		return dedupeEntry{}, false
	}
	return e, true
}

// remember records a guild-wide verdict against this text. A nil verdict
// means clean.
func (c *dedupeCache) remember(guildID, content string, now time.Time, v *cachedVerdict) {
	c.rememberKey(dedupeKey(guildID, "", content), now, v)
}

func (c *dedupeCache) rememberKey(key uint64, now time.Time, v *cachedVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	c.seen[key] = dedupeEntry{at: now, verdict: v}
}

// markClean records that this text was scanned and had nothing against it,
// so identical copies inside the window cost nothing. Copypasta, a member
// repeating themselves and a raid posting one line from fifty accounts all
// collapse to a single model call, which is where most of the saving is.
//
// Swept on write rather than on a ticker: no goroutine to leak, and the
// sweep only runs when the map is actually growing.
func (c *dedupeCache) markClean(guildID, content string, now time.Time) {
	c.remember(guildID, content, now, nil)
}

// markCleanFor records a clear that applies to one member only.
//
// The asymmetry is the point, and it fixes a false negative the verdict cache
// introduced. A guild-wide clear is right for the fast pass, which clears
// text on generic grounds ("nothing here"), and that is where the raid saving
// comes from: fifty accounts posting one clean line cost one call.
//
// A deep-pass clear is not generic. That pass clears on speaker-dependent
// grounds, which is exactly what hate_speech's reclaimed-language line and
// every "these two are obviously friends" reading are. Shared guild-wide,
// one member being cleared for a slur meant the identical slur from anybody
// else was skipped for the rest of dedupeWindow, which is the reported
// failure with a cache in front of it.
//
// Acting is not narrowed the same way, and should not be: repetition is
// aggravating, and a verdict that something IS a violation holds against
// whoever posts it.
func (c *dedupeCache) markCleanFor(guildID, authorID, content string, now time.Time) {
	c.rememberKey(dedupeKey(guildID, authorID, content), now, nil)
}

// sweepLocked bounds the map. Swept on write rather than on a ticker: no
// goroutine to leak, and it only runs when the map is actually growing.
func (c *dedupeCache) sweepLocked(now time.Time) {
	if len(c.seen) < dedupeMax {
		return
	}
	for k, e := range c.seen {
		if now.Sub(e.at) >= dedupeWindow {
			delete(c.seen, k)
		}
	}
	// Still full after sweeping means the window is genuinely saturated (a
	// raid). Drop everything rather than growing: the cost is that the next
	// few duplicates get classified again, which is the safe direction.
	if len(c.seen) >= dedupeMax {
		clear(c.seen)
	}
}

// shouldSkip runs rung 0: everything that can be decided from the message
// and the guild's config alone, with no scanning and no cost.
func (p *Plugin) shouldSkip(cfg Config, m *discordgo.Message, now time.Time) skipReason {
	if cfg.Mode == ModeOff {
		return skipMode
	}
	// Webhook messages first among the author checks, because this is also
	// the loop guard: a rewrite reposts through a webhook, that repost comes
	// straight back as a MessageCreate, and without this the plugin would
	// classify its own output forever.
	if m.WebhookID != "" {
		return skipWebhook
	}
	if m.Author == nil || m.Author.Bot {
		return skipBot
	}
	if slices.Contains(cfg.ExemptChannelIDs, m.ChannelID) {
		return skipChannel
	}
	if m.Member != nil {
		for _, roleID := range m.Member.Roles {
			if slices.Contains(cfg.ExemptRoleIDs, roleID) {
				return skipRole
			}
		}
	}
	// Not m.Content: a forwarded message carries its text in a snapshot and
	// leaves Content empty, so reading Content here returned skipEmpty and
	// let the forward button bypass every rung below. See messageText.
	text, _ := messageText(m)
	content := strings.TrimSpace(text)
	if content == "" {
		return skipEmpty
	}
	if len([]rune(content)) < minScanLen {
		return skipShort
	}
	if p.dedupe.seenClean(m.GuildID, m.Author.ID, content, now) {
		return skipDuplicate
	}
	return skipNone
}

// Rung 1: patterns unambiguous enough to act on with no model involved.
//
// Deliberately tiny. Regex is a spam gate, not a policy engine, and every
// pattern here is one whose false-positive rate is near zero on ordinary
// conversation. Anything needing judgement belongs to the model rungs, where
// a wrong answer at least got a chance to read the sentence.
//
// The cost of getting this list wrong is high and silent, so the bar for
// adding to it is: could a member post this innocently? If yes, it is not a
// hard hit.
//
// Go's regexp is RE2, which has no backtracking and therefore no lookahead.
// Anything wanting "match this but not that" needs the exclusion as its own
// pattern in notIf, checked after pattern matches. Writing it as a lookahead
// compiles to a panic at package init, which is to say at every startup.
var hardPatterns = []struct {
	bucket  Bucket
	reason  string
	pattern *regexp.Regexp
	// notIf, when set and matching, cancels the hit. For narrowing a shape
	// that is right most of the time but has known innocent forms.
	notIf *regexp.Regexp
}{
	{
		bucket: BucketMalicious,
		reason: "Discord bot token in message text",
		// A leaked bot token is the one credential shape that is both
		// unmistakable and urgent: anyone who reads it owns the bot.
		pattern: regexp.MustCompile(`\b[A-Za-z0-9_-]{24,28}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{27,}\b`),
	},
	{
		bucket:  BucketMalicious,
		reason:  "known Discord phishing domain pattern",
		pattern: regexp.MustCompile(`(?i)https?://[^\s/]*\b(discord|dlscord|discrod|discocrd|steamcommunlty|discordgift|dicsord)[a-z0-9-]*\.(ru|cf|gq|tk|ml|ga|xyz|top|click|link|monster|shop)\b`),
	},
	{
		bucket: BucketMalicious,
		reason: "IP grabber or logger link",
		// These services exist for one purpose and are named after it.
		pattern: regexp.MustCompile(`(?i)https?://(?:[a-z0-9-]+\.)?(grabify\.link|iplogger\.(org|com|ru)|blasze\.com|yip\.su|2no\.co|iplis\.ru|ps3cfw\.com)\b`),
	},
	{
		bucket: BucketDoxxing,
		reason: "government identity number",
		// A US social security number in the standard dashed form. Narrow on
		// purpose: bare nine-digit strings are order numbers as often as they
		// are anything else, so only the dashed 3-2-4 shape counts.
		pattern: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
		// The group ranges the US never issues, which is exactly what somebody
		// reaches for when writing a fake one to illustrate a point. Excluding
		// them costs nothing, because a real SSN cannot look like this, and it
		// stops the filter firing on somebody explaining what an SSN is.
		notIf: regexp.MustCompile(`\b(000|666|9\d\d)-\d{2}-\d{4}\b|\b\d{3}-00-\d{4}\b|\b\d{3}-\d{2}-0000\b`),
	},
}

// sub is one replacement: the singular form and the plural, picked by how
// the matched word ended.
type sub struct{ one, many string }

// hardSlurs are the words Discord's guidelines treat as a violation on
// sight. They sit in rung 1 rather than with the model rungs because they
// are the one part of hate speech that needs no sentence read around it:
// there is no framing in which these are the word somebody reached for by
// accident, and waiting for a model to agree costs a call and a second.
//
// Unlike everything above, a hit here is *rewritable*: the violation is a
// word rather than the whole message, so replacing it leaves an otherwise
// ordinary sentence standing. The replacements are deliberately silly. A
// member who tries again gets the same treatment and no argument, which is
// a cheaper outcome for a moderator than a removal somebody appeals.
//
// Obfuscation tolerance stops at digit-for-letter substitution and doubled
// letters. Chasing spacing and unicode homoglyphs here would start costing
// false positives, and anything that gets past this is still read by the
// model rungs, which is what they are for.
var hardSlurs = []struct {
	pattern *regexp.Regexp
	// subs is picked from at random per match, so a member spamming one
	// word gets a different daft answer every time and nothing to argue
	// with. Any of them is publishable, which is what the switch below
	// relies on.
	subs []sub
}{
	{
		regexp.MustCompile(`(?i)\bn[i1]gg+[e3]r+(s|z)?\b`),
		[]sub{
			{"ninja", "ninjas"},
			{"ninjago", "ninjagos"},
			{"nice person", "nice people"},
			{"night owl", "night owls"},
			{"nintendo enjoyer", "nintendo enjoyers"},
		},
	},
	{
		regexp.MustCompile(`(?i)\bf[a4]gg+[o0]t+(s|z)?\b`),
		[]sub{
			{"frog", "frogs"},
			{"fog", "fogs"},
			{"thot", "thots"},
			{"fine gentleman", "fine gentlemen"},
			{"forklift certified individual", "forklift certified individuals"},
		},
	},
	{
		regexp.MustCompile(`(?i)\btr[a4]nn(y|ies|ys)\b`),
		[]sub{
			{"person", "people"},
			{"nice person", "nice people"},
			{"epic person", "epic people"},
			{"transformer", "transformers"},
			{"trombone player", "trombone players"},
		},
	},
	{
		regexp.MustCompile(`(?i)\btr[o0]{2,}n(s|z)?\b`),
		[]sub{
			{"person", "people"},
			{"nice person", "nice people"},
			{"epic person", "epic people"},
			{"cartoon", "cartoons"},
			{"trooper", "troopers"},
		},
	},
}

// redactSlurs replaces every hard slur in content, reporting whether any
// matched. The replacement is what gets published, so it is built from the
// member's own message rather than from anything a model returned.
func redactSlurs(content string) (string, bool) {
	out, hit := content, false
	for _, s := range hardSlurs {
		out = s.pattern.ReplaceAllStringFunc(out, func(m string) string {
			hit = true
			r := s.subs[rand.IntN(len(s.subs))]
			switch m[len(m)-1] {
			case 's', 'S', 'z', 'Z':
				return r.many
			}
			return r.one
		})
	}
	return out, hit
}

// hardHit runs rung 1. Returns the first matching pattern's bucket and
// reason, or an empty bucket for no match. A non-empty rewrite is the
// publishable version of the message: only the slur patterns produce one,
// since a credential or a phishing link has no cleaned-up form.
func hardHit(content string) (bucket Bucket, reason, rewrite string, hit bool) {
	for _, h := range hardPatterns {
		if !h.pattern.MatchString(content) {
			continue
		}
		if h.notIf != nil && h.notIf.MatchString(content) {
			continue
		}
		return h.bucket, h.reason, "", true
	}
	if clean, ok := redactSlurs(content); ok {
		return BucketHateSpeech, "hard slur", clean, true
	}
	return "", "", "", false
}

// enforcedBuckets is the set the fast pass is told to look for: everything
// this guild has set to something other than off.
//
// Sending only these is a policy decision and a cost saving at the same
// time. A guild with four buckets on has a prompt roughly half the size of
// one with ten, and the model is not being asked to form a view on
// categories nobody wants enforced.
func enforcedBuckets(cfg Config) []Bucket {
	var out []Bucket
	for _, b := range AllBuckets {
		if EffectiveAction(cfg.BucketActions, b) != ActionOff {
			out = append(out, b)
		}
	}
	return out
}
