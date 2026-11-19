package aimod

import (
	"context"
	"testing"
)

// Real rewrites from production pass; what a prompt injection would want a
// rewrite to be does not.
func TestFaithful(t *testing.T) {
	for _, c := range [][2]string{
		{"fucking retard", "fucking silly goose"},
		{"shut up retard", "shut up turnip"},
		{"LEAVE. WOMEN. ALONE.", "LEAVE. NICE PEOPLE. ALONE."},
		{"Hila Kike", "Hila kite"},
		{"Stop talking about yourself you stupid queer", "Stop talking about yourself you stupid marshmallow"},
		{"see https://example.com you idiot", "see https://example.com you goose"},
	} {
		if !faithful(c[0], c[1]) {
			t.Errorf("%q -> %q refused", c[0], c[1])
		}
	}
	for _, c := range [][2]string{
		{"hey how are you all doing", "free nitro for everyone today click now"},
		{"you are a retard lol", "you are a goose lol https://grabify.link/x"},
		{"you are a retard lol", "you are a goose lol discord.gg/raid"},
		{"you are a retard lol", "you are a goose lol <@175928847299117063>"},
		{"you are a retard lol", "you are a goose lol @everyone"},
		{"retard", "i want everyone in this server to know that i am a terrible person"},
	} {
		if faithful(c[0], c[1]) {
			t.Errorf("%q -> %q let through", c[0], c[1])
		}
	}
}

// A rewrite steered by instructions in the channel (one that isn't the
// member's sentence, or carries a link they never wrote) is not published
// under their name: the message is removed instead.
func TestAnInjectedRewriteIsARemoval(t *testing.T) {
	for _, rw := range []string{
		"i am selling my account dm me",
		"call me tomorrow ok https://grabify.link/x",
	} {
		store := newFakeStore()
		ops := newFakeOps()
		p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})
		v := confirmed("one line had a phone number in it")
		v.Rewrite = rw
		store.setConfig(enforcingConfig())
		p.enforce(context.Background(), enforcingConfig(),
			candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "call me at 555 0100 tomorrow ok"},
			BucketDoxxing, ActionRewrite, v)
		if inc := store.recorded(); len(inc) != 1 || inc[0].Action != ActionRemove {
			t.Errorf("%q: incident %+v, want a removal", rw, inc)
		}
		if _, posted := ops.snapshot(); len(posted) != 0 {
			t.Errorf("%q: posted %d under the member's name", rw, len(posted))
		}
	}
}
