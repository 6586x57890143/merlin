package aimod

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The example guild: "work" and "employment" are a running joke there.
var jokeList = []BannedWord{
	{Word: "work", Replacement: "the mines"},
	{Word: "employment", Replacement: "indentured servitude"},
}

func TestRedactWords(t *testing.T) {
	cases := []struct{ in, want string }{
		{"off to work now", "off to the mines now"},
		{"Work sucks", "The mines sucks"},
		{"w.o.r.k and w0rk and woork", "the mines and the mines and the mines"},
		{"working late, workers unite", "the minesing late, the minesers unite"},
		{"seeking employment", "seeking indentured servitude"},
		// Neither end may be glued to another word.
		{"the network is down", "the network is down"},
		{"homework at the workshop", "homework at the workshop"},
		{"unemployment", "unemployment"},
		// Letters lining up across ordinary words are not the word.
		{"two rk", "two rk"},
	}
	for _, c := range cases {
		got, hit := redactWords(jokeList, c.in)
		if got != c.want || hit != (c.in != c.want) {
			t.Errorf("redactWords(%q) = %q, %v; want %q", c.in, got, hit, c.want)
		}
	}

	// No replacement removes the whole message.
	if got, hit := redactWords([]BannedWord{{Word: "work"}}, "back to work"); !hit || got != "" {
		t.Errorf("remove entry gave %q, %v; want empty and a hit", got, hit)
	}
}

func TestNormalizeWord(t *testing.T) {
	if w, err := normalizeWord("  Day   Job "); err != nil || w != "day job" {
		t.Errorf("normalizeWord = %q, %v", w, err)
	}
	// Anything slurRe would read as its own syntax, or split mid-rune, is
	// refused rather than compiled.
	for _, bad := range []string{"ab", "wo[rk", "w(o)rk", "wo?rk", "仕事", strings.Repeat("a", 41)} {
		if _, err := normalizeWord(bad); err == nil {
			t.Errorf("normalizeWord(%q) accepted", bad)
		}
	}
}

func TestWordListRewritesWithoutSanctionOrModel(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	classifier := &fakeClassifier{}
	jailer := &fakeJailer{}
	p := testPlugin(t, store, classifier, ops, &fakeAudit{})
	p.jailer = jailer

	cfg := sanctioningConfig()
	cfg.WordList = jokeList
	store.setConfig(cfg)

	p.HandleMessage(&discordgo.Message{
		ID: "m1", GuildID: cfg.GuildID, ChannelID: "c1",
		Content: "anyone hiring? I need employment",
		Author:  &discordgo.User{ID: "u1"}, Member: &discordgo.Member{},
	})
	p.wg.Wait()

	deleted, posted := ops.snapshot()
	if len(deleted) != 1 || len(posted) != 1 {
		t.Fatalf("deleted %v, posted %d, want the message replaced", deleted, len(posted))
	}
	if !strings.HasPrefix(posted[0].Content, "anyone hiring? I need indentured servitude") {
		t.Errorf("reposted %q", posted[0].Content)
	}
	if len(jailer.jailed()) != 0 {
		t.Errorf("a word-list hit sanctioned somebody")
	}
	if n, _ := store.CountSanctions(context.Background(), cfg.GuildID, "u1", p.now().Add(-time.Hour)); n != 0 {
		t.Errorf("a word-list hit counts toward future sanctions (%d)", n)
	}

	// Another guild with no list leaves the same words alone.
	other := sanctioningConfig()
	other.GuildID = "g2"
	store.setConfig(other)
	p.HandleMessage(&discordgo.Message{
		ID: "m2", GuildID: "g2", ChannelID: "c2",
		Content: "back to work", Author: &discordgo.User{ID: "u1"}, Member: &discordgo.Member{},
	})
	p.wg.Wait()
	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("a guild without the word acted on it: deleted %v", deleted)
	}
}

func TestWordsAddRemove(t *testing.T) {
	store := newFakeStore()
	store.setConfig(enforcingConfig())
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	s := testSession(t)
	ctx := context.Background()

	p.handleWordsAdd(ctx, s, interaction("g1", "words", "add", strOpt("word", "Work"), strOpt("replacement", "the mines")))
	p.handleWordsAdd(ctx, s, interaction("g1", "words", "add", strOpt("word", "work"), strOpt("replacement", "jorking"), boolOpt("noun_only", true)))
	// Refused: the replacement would be rewritten again on the way out.
	p.handleWordsAdd(ctx, s, interaction("g1", "words", "add", strOpt("word", "job"), strOpt("replacement", "a job")))
	cfg, _ := store.Config(ctx, "g1")
	if len(cfg.WordList) != 1 || cfg.WordList[0] != (BannedWord{Word: "work", Replacement: "jorking", NounOnly: true}) {
		t.Fatalf("word list = %v, want work replaced once", cfg.WordList)
	}

	p.handleWordsList(ctx, s, interaction("g1", "words", "list"))
	if got := p.autocompleteWord(ctx, interaction("g1", "words", "remove"), "word", "wo"); len(got) != 1 {
		t.Errorf("autocomplete = %v", got)
	}

	p.handleWordsRemove(ctx, s, interaction("g1", "words", "remove", strOpt("word", "WORK")))
	cfg, _ = store.Config(ctx, "g1")
	if len(cfg.WordList) != 0 {
		t.Errorf("word list = %v after remove", cfg.WordList)
	}
}

// Against Postgres: the list survives a round trip, and a word-list incident
// never counts toward the escalation ladder.
func TestWordListStore(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.SetWordList(ctx, "g1", jokeList); err != nil {
		t.Fatalf("SetWordList: %v", err)
	}
	cfg, err := s.Config(ctx, "g1")
	if err != nil || len(cfg.WordList) != 2 || cfg.WordList[1] != jokeList[1] {
		t.Fatalf("Config = %v, %v", cfg.WordList, err)
	}
	if err := s.SetWordList(ctx, "g1", nil); err != nil {
		t.Fatalf("SetWordList(nil): %v", err)
	}
	if cfg, _ = s.Config(ctx, "g1"); len(cfg.WordList) != 0 {
		t.Errorf("cleared list read back as %v", cfg.WordList)
	}

	now := time.Now().UTC()
	if _, err := s.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketWordList, Action: ActionRewrite, CreatedAt: now,
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	if n, err := s.CountSanctions(ctx, "g1", "u1", now.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("CountSanctions = %d, %v; a word-list hit must not count", n, err)
	}
}

func TestNounOnly(t *testing.T) {
	list := []BannedWord{{Word: "work", Replacement: "the mines", NounOnly: true}}
	cases := []struct{ in, want string }{
		// Verbs pass.
		{"this works", "this works"},
		{"this doesn't work", "this doesn't work"},
		{"this doesn" + string(rune(0x2019)) + "t work", "this doesn" + string(rune(0x2019)) + "t work"},
		{"does this work?", "does this work?"},
		{"I work from home", "I work from home"},
		{"need to work on it", "need to work on it"},
		{"it worked", "it worked"},
		{"working late", "working late"},
		{"make it work", "make it work"},
		// Nouns go.
		{"at work rn", "at the mines rn"},
		{"going to work", "going to the mines"},
		{"back to work", "back to the mines"},
		{"my work is boring", "my the mines is boring"},
		{"Work sucks", "The mines sucks"},
		{"done. work tomorrow", "done. the mines tomorrow"},
		{"this work is great", "this the mines is great"},
		{"they work at work", "they work at the mines"},
	}
	for _, c := range cases {
		if got, _ := redactWords(list, c.in); got != c.want {
			t.Errorf("redactWords(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
