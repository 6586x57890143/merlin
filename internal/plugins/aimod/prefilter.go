package aimod

import (
	"hash/fnv"
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
// A judgement call with a real cost either way, so: short messages are where
// context does all the work ("kys" is three characters and is a genuine
// self_harm or threats hit), which argues for zero. But a filter that sends
// every "lol" and "yeah" to a model spends most of its budget on the
// cheapest possible nothing. The compromise is that regexHits below runs on
// every message regardless of length, so the short strings that genuinely
// matter are caught by pattern rather than skipped, and the length gate only
// suppresses the model call.
const minScanLen = 12

// dedupeWindow is how long an identical message counts as already decided.
//
// Copypasta, a member repeating themselves, and a raid posting the same line
// from fifty accounts all collapse to one model call inside this window. The
// window is short because the same text genuinely can mean different things
// in different conversations, and long enough to cover the shape a raid
// actually takes.
const dedupeWindow = 5 * time.Minute

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
type dedupeCache struct {
	mu   sync.Mutex
	seen map[uint64]time.Time
}

func newDedupeCache() *dedupeCache {
	return &dedupeCache{seen: make(map[uint64]time.Time)}
}

// seenRecently reports whether this text was decided inside the window, and
// records it either way. Swept on write rather than on a ticker: there is no
// goroutine to leak, and the sweep only runs when the map is actually
// growing.
func (c *dedupeCache) seenRecently(guildID, content string, now time.Time) bool {
	h := fnv.New64a()
	// Guild-scoped so two servers running the same copypasta do not mask
	// each other, and because a verdict is only meaningful against one
	// guild's policy anyway.
	_, _ = h.Write([]byte(guildID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(content))))
	key := h.Sum64()

	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.seen[key]; ok && now.Sub(at) < dedupeWindow {
		return true
	}
	if len(c.seen) >= dedupeMax {
		for k, at := range c.seen {
			if now.Sub(at) >= dedupeWindow {
				delete(c.seen, k)
			}
		}
		// Still full after sweeping means the window is genuinely saturated
		// (a raid). Drop everything rather than growing: the cost is that
		// the next few duplicates get scanned, which is the safe direction.
		if len(c.seen) >= dedupeMax {
			clear(c.seen)
		}
	}
	c.seen[key] = now
	return false
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
	content := strings.TrimSpace(m.Content)
	if content == "" {
		return skipEmpty
	}
	if len([]rune(content)) < minScanLen {
		return skipShort
	}
	if p.dedupe.seenRecently(m.GuildID, content, now) {
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

// hardHit runs rung 1. Returns the first matching pattern's bucket and
// reason, or an empty bucket for no match.
func hardHit(content string) (Bucket, string, bool) {
	for _, h := range hardPatterns {
		if !h.pattern.MatchString(content) {
			continue
		}
		if h.notIf != nil && h.notIf.MatchString(content) {
			continue
		}
		return h.bucket, h.reason, true
	}
	return "", "", false
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
