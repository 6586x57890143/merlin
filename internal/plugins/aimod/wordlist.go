package aimod

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// A guild's own word list: words this one server does not want, rewritten
// to something it chose or removed. It sits between rung 0 and rung 1 and is
// free, like both.
//
// It is house style, not Discord policy, and everything below follows from
// that. It is per guild because one server turning "work" into a joke would
// be vandalism on every other. It records under BucketWordList, which is
// deliberately not in AllBuckets, so no prompt, policy file or calibration
// ever hears of it. And it never sanctions, never reaches the rap sheet and
// never counts toward one: a member who said "employment" on a server with a
// running gag has broken no rule anybody else would recognise.

// BucketWordList is what a word-list hit is recorded under.
const BucketWordList Bucket = "word_list"

// BannedWord is one entry. An empty Replacement removes the message.
//
// NounOnly is for a word that is also an ordinary verb: "work" the place
// should go, "this works" and "doesn't work" should not. It matches only the
// bare word (no suffixes, so "works" and "working" always pass) and skips it
// when the words just before it say verb (see verbBefore).
type BannedWord struct {
	Word        string `json:"word"`
	Replacement string `json:"replacement,omitempty"`
	NounOnly    bool   `json:"noun_only,omitempty"`
}

const (
	// maxWordList bounds the per-message cost: every entry is one more
	// regexp run over every message in the guild.
	maxWordList       = 50
	minWordLen        = 3
	maxWordLen        = 40
	maxReplacementLen = 100
)

// wordShape is what a guild may list: ASCII letters and digits, single
// spaces between words. ponytail: ASCII only, because slurRe walks the
// spelling byte by byte and would split a multibyte letter into an invalid
// pattern; widen slurRe to runes if a guild needs another script.
var wordShape = regexp.MustCompile(`^[a-z0-9]+(?: [a-z0-9]+)*$`)

// wordSuffixes are the endings a listed word may carry and still count as
// that word. Banning "work" should catch "working" and "workers"; it must
// not catch "network" or "workshop", which is why the match is anchored at
// the start of a word and the tail is checked against this rather than left
// open the way the slur patterns leave it.
var wordSuffixes = map[string]bool{"": true, "s": true, "es": true, "d": true, "ed": true, "er": true, "ers": true, "ing": true}

// wordPatterns caches compiled entries across guilds and messages: this runs
// on every message, and the list only changes when an admin edits it.
// ponytail: never evicted; bounded by the distinct words ever listed, which
// is tiny next to anything else this process holds.
var wordPatterns sync.Map

func wordPattern(word string) *regexp.Regexp {
	if re, ok := wordPatterns.Load(word); ok {
		return re.(*regexp.Regexp)
	}
	// slurRe is the strong part: separators between letters, repeated
	// letters and the usual digit-for-vowel swaps all land, exactly as they
	// do for the built-in slurs. wordShape keeps its spec syntax out.
	re := slurRe(word)
	wordPatterns.Store(word, re)
	return re
}

// normalizeWord folds, lowercases and collapses whitespace, and reports
// whether the result is listable. Folded so an admin pasting a term in
// fullwidth or with an accent lists the plain word redactWords matches
// against, rather than being refused.
func normalizeWord(s string) (string, error) {
	w := strings.Join(strings.Fields(strings.ToLower(fold(s))), " ")
	switch {
	case len(w) < minWordLen || len(w) > maxWordLen:
		return "", fmt.Errorf("a listed word has to be %d to %d characters", minWordLen, maxWordLen)
	case !wordShape.MatchString(w):
		return "", fmt.Errorf("a listed word can only use the letters a to z, digits and spaces")
	}
	return w, nil
}

// redactWords applies a guild's list to text, reporting whether anything
// matched. An empty result with a hit means an entry with no replacement
// matched, so the whole message goes: the same convention the deep pass and
// redactSlurs use.
//
// Folded first, as redactSlurs is, so a listed word in fullwidth or Cyrillic
// still lands; a miss returns content untouched.
func redactWords(list []BannedWord, content string) (string, bool) {
	out, hit := fold(content), false
	for _, w := range list {
		re := wordPattern(w.Word)
		var b strings.Builder
		last := 0
		for pos := 0; pos < len(out); {
			loc := re.FindStringIndex(out[pos:])
			if loc == nil {
				break
			}
			start, end := pos+loc[0], trimTail(out, pos+loc[0], pos+loc[1])
			ws, we := wordBounds(out, start, end)
			suffix := strings.ToLower(out[end:we])
			if ws < start || !wordSuffixes[suffix] || acrossWords(out[start:end], false) ||
				(w.NounOnly && (suffix != "" || verbBefore(out[:start]))) {
				_, size := utf8.DecodeRuneInString(out[start:])
				pos = start + size
				continue
			}
			if w.Replacement == "" {
				return "", true
			}
			b.WriteString(out[last:start])
			b.WriteString(matchCase(out[start:end], w.Replacement))
			last, pos, hit = end, end, true
		}
		b.WriteString(out[last:])
		out = b.String()
	}
	if !hit {
		return content, false
	}
	return out, true
}

// verbSignals are words that, right before a noun-only word, make it a verb:
// subject pronouns ("I work"), auxiliaries and modals ("doesn't work", "will
// work"), and adverbs that sit between the two ("never work", "just work").
// ponytail: a word list, not a part-of-speech tagger. It misses imperatives
// at the start of a sentence ("Work harder") and errs toward replacing,
// which for a joke list is the cheaper mistake.
var verbSignals = map[string]bool{
	"i": true, "you": true, "we": true, "they": true, "he": true, "she": true, "it": true,
	"do": true, "does": true, "did": true, "don't": true, "doesn't": true, "didn't": true,
	"dont": true, "doesnt": true, "didnt": true, "not": true,
	"will": true, "won't": true, "wont": true, "would": true, "wouldn't": true,
	"can": true, "can't": true, "cant": true, "could": true, "couldn't": true,
	"should": true, "shouldn't": true, "must": true, "might": true, "may": true,
	"gonna": true, "wanna": true, "gotta": true, "let's": true, "lets": true, "please": true,
	"never": true, "always": true, "just": true, "still": true, "also": true, "really": true, "actually": true,
}

// commuteVerbs are what turn a following "to" back into a noun: "go to
// work", "back to work". Any other "to" is an infinitive ("need to work").
var commuteVerbs = map[string]bool{
	"go": true, "goes": true, "going": true, "went": true, "gone": true, "back": true,
	"get": true, "gets": true, "got": true, "getting": true,
	"come": true, "comes": true, "came": true, "coming": true, "late": true, "head": true, "heading": true,
}

// verbBefore reads the two words in front of a match, within its sentence.
func verbBefore(before string) bool {
	prev, rest := lastWord(before)
	switch prev {
	case "to":
		w, _ := lastWord(rest)
		return !commuteVerbs[w]
	case "this", "that", "these", "those":
		// "does this work?" is a verb, "this work is great" a noun.
		w, _ := lastWord(rest)
		return verbSignals[w] && !isPronoun(w)
	}
	return verbSignals[prev]
}

func isPronoun(w string) bool {
	switch w {
	case "i", "you", "we", "they", "he", "she", "it":
		return true
	}
	return false
}

// lastWord returns the final word of s, lowercased with its edge punctuation
// trimmed, and everything before it. A sentence end in between means there
// is no previous word: "Done. Work is fun" starts afresh at "Work".
func lastWord(s string) (string, string) {
	s = strings.TrimRight(s, " \t\n")
	i := strings.LastIndexAny(s, " \t\n")
	tok := s[i+1:]
	if strings.ContainsAny(tok[max(0, len(tok)-1):], ".!?") {
		return "", ""
	}
	// Phones type the curly apostrophe: "doesn't" either way.
	tok = strings.ReplaceAll(strings.ToLower(tok), string(rune(0x2019)), "'")
	return strings.Trim(tok, tokenEdge), s[:max(0, i)]
}

// matchCase capitalises the replacement when the word it replaces started
// with a capital, so a sentence still starts like one.
func matchCase(matched, replacement string) string {
	r, _ := utf8.DecodeRuneInString(matched)
	if !unicode.IsUpper(r) {
		return replacement
	}
	first, size := utf8.DecodeRuneInString(replacement)
	return string(unicode.ToUpper(first)) + replacement[size:]
}

// actOnWordList enforces a word-list hit and reports whether there was one.
func (p *Plugin) actOnWordList(cfg Config, c candidate) bool {
	rewrite, hit := redactWords(cfg.WordList, c.Content)
	if !hit {
		return false
	}
	action := ActionRewrite
	if rewrite == "" {
		action = ActionRemove
	}
	p.spawn(func(bg context.Context) {
		p.enforce(bg, cfg, c, BucketWordList, action, deepVerdict{
			Violation: true, Bucket: BucketWordList, Confidence: 1,
			Reason: "a word on this server's own word list", Rewrite: rewrite,
		})
	})
	return true
}

func (p *Plugin) handleWordsAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	word, err := normalizeWord(opts["word"].StringValue())
	if err != nil {
		core.RespondErr(s, i, "Can't list that", err)
		return
	}
	entry := BannedWord{Word: word}
	if o, ok := opts["replacement"]; ok {
		entry.Replacement = strings.TrimSpace(o.StringValue())
	}
	if o, ok := opts["noun_only"]; ok {
		entry.NounOnly = o.BoolValue()
	}
	if len(entry.Replacement) > maxReplacementLen {
		core.RespondErr(s, i, "Can't list that", fmt.Errorf("the replacement can be at most %d characters", maxReplacementLen))
		return
	}
	// A replacement that is itself a hit would be rewritten again by the
	// pass enforce runs over every rewrite before it is posted.
	if _, self := redactWords([]BannedWord{entry}, entry.Replacement); self {
		core.RespondErr(s, i, "Can't list that", fmt.Errorf("the replacement contains the word it replaces"))
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	next := make([]BannedWord, 0, len(cfg.WordList)+1)
	for _, w := range cfg.WordList {
		if w.Word != word {
			next = append(next, w)
		}
	}
	if len(next) >= maxWordList {
		core.RespondErr(s, i, "Word list full", fmt.Errorf("a server can list at most %d words", maxWordList))
		return
	}
	next = append(next, entry)
	if err := p.store.SetWordList(ctx, i.GuildID, next); err != nil {
		core.RespondErr(s, i, "Failed to update the word list", err)
		return
	}
	// Text already judged clean is remembered for dedupeWindow, and that
	// verdict predates this word.
	p.dedupe.reset()
	p.auditConfig(ctx, i, "aimod.word_list", "", "added "+describeWord(entry))
	core.RespondOK(s, i, "Word listed", describeWord(entry)+
		"\nApplies to new and edited messages from now on. Exempt channels and roles are still exempt.")
}

func (p *Plugin) handleWordsRemove(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	word := strings.Join(strings.Fields(strings.ToLower(core.LeafArgs(i)["word"].StringValue())), " ")
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	next := make([]BannedWord, 0, len(cfg.WordList))
	for _, w := range cfg.WordList {
		if w.Word != word {
			next = append(next, w)
		}
	}
	if len(next) == len(cfg.WordList) {
		core.RespondInfo(s, i, "No change", fmt.Sprintf("%q isn't on this server's word list.", word))
		return
	}
	if err := p.store.SetWordList(ctx, i.GuildID, next); err != nil {
		core.RespondErr(s, i, "Failed to update the word list", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.word_list", "", fmt.Sprintf("removed %q", word))
	core.RespondOK(s, i, "Word removed", fmt.Sprintf("%q is no longer on this server's word list.", word))
}

func (p *Plugin) handleWordsList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	if len(cfg.WordList) == 0 {
		core.RespondInfo(s, i, "Word list", "Nothing listed. Add one with `/aimod words add`.")
		return
	}
	lines := make([]string, 0, len(cfg.WordList))
	for _, w := range cfg.WordList {
		lines = append(lines, "- "+describeWord(w))
	}
	core.RespondInfo(s, i, fmt.Sprintf("Word list (%d)", len(cfg.WordList)),
		core.TruncateEmbedDescription(strings.Join(lines, "\n")))
}

func (p *Plugin) autocompleteWord(ctx context.Context, i *discordgo.InteractionCreate, _, focused string) []*discordgo.ApplicationCommandOptionChoice {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		return nil
	}
	needle := strings.ToLower(focused)
	var out []*discordgo.ApplicationCommandOptionChoice
	for _, w := range cfg.WordList {
		if len(out) == 25 {
			break
		}
		if strings.Contains(w.Word, needle) {
			out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: w.Word, Value: w.Word})
		}
	}
	return out
}

func describeWord(w BannedWord) string {
	out := fmt.Sprintf("`%s` becomes `%s`", w.Word, w.Replacement)
	if w.Replacement == "" {
		out = fmt.Sprintf("`%s`: message removed", w.Word)
	}
	if w.NounOnly {
		out += " (as a noun only)"
	}
	return out
}
