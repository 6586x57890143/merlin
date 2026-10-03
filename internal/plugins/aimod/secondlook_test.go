package aimod

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// A regex slur hit is checked by the deep model before it is acted on, and
// only a confident "these are ordinary words" stands it down. The model sees
// the matched words and nothing else from the message.
func TestSecondLookGatesRegexSlurHits(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		answer     string
		wantCalls  int
		wantActed  bool
		wantPrompt string
	}{
		// A match glued inside a longer word is the case the look is for.
		{"confident clear stands rung 1 down", "this slime is gooky", `{"slur":false,"confidence":0.95}`, 1, false, "gooky"},
		{"a hesitant clear does not", "this slime is gooky", `{"slur":false,"confidence":0.5}`, 1, true, "gooky"},
		{"a confirmation acts", "this slime is gooky", `{"slur":true,"confidence":0.99}`, 1, true, "gooky"},
		{"an unparseable answer acts", "this slime is gooky", `not json`, 1, true, "gooky"},
		// A slur from end to end has no letters lining up by accident, so it
		// is never asked: a model may well call "faggot" a bundle of sticks.
		{"the n-word by shape is never second-guessed", "lol Nibber", `{"slur":false,"confidence":1}`, 0, true, ""},
		{"a whole-word slur is never second-guessed", "shut up you faggot", `{"slur":false,"confidence":1}`, 0, true, ""},
		{"nor when the next word starts with an s", "Shut up faggot sissy retard", `{"slur":false,"confidence":1}`, 0, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, ops := newFakeStore(), newFakeOps()
			cls := &fakeClassifier{secondLook: []string{tc.answer}}
			p := intakePlugin(t, store, cls, ops)
			cfg, _ := store.Config(t.Context(), "g1")
			cfg.BucketActions[BucketHateSpeech] = ActionRewrite
			store.setConfig(cfg)

			p.HandleMessage(&discordgo.Message{
				ID: "m1", GuildID: "g1", ChannelID: "c1", Content: tc.content,
				Author: &discordgo.User{ID: "u1"}, Member: &discordgo.Member{},
			})
			p.wg.Wait()

			if cls.secondLookCalls != tc.wantCalls {
				t.Fatalf("second look called %d times, want %d", cls.secondLookCalls, tc.wantCalls)
			}
			deleted, _ := ops.snapshot()
			if acted := len(deleted) > 0; acted != tc.wantActed {
				t.Errorf("acted = %v, want %v", acted, tc.wantActed)
			}
			if tc.wantPrompt != "" {
				if got := cls.lastSecondLookReq.Messages[1].Content; got != tc.wantPrompt {
					t.Errorf("model was shown %q, want only the matched words %q", got, tc.wantPrompt)
				}
			}
		})
	}
}

func TestReplacedWords(t *testing.T) {
	got := replacedWords("you you faggot ok", "you you frog ok")
	if len(got) != 1 || got[0] != "faggot" {
		t.Errorf("replacedWords = %q, want [faggot]", got)
	}
}
