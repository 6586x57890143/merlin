package core

import (
	"bytes"
	cards "github.com/6586x57890143/cygnet/support"
	"testing"
)

func TestPaymentMarksUseTheSharedApplicationEmojiLibrary(t *testing.T) {
	all := map[string]Emoji{}
	for _, e := range Emojis() {
		if _, duplicate := all[e.Key]; duplicate {
			t.Fatalf("duplicate emoji %s", e.Key)
		}
		all[e.Key] = e
	}
	for _, key := range cards.Icons {
		raw, ok := cards.IconPNG(key)
		e, found := all["support_"+key]
		if !ok || !found || !bytes.Equal(e.PNG, raw) {
			t.Errorf("shared mark %s missing or out of date", key)
		}
	}
}
