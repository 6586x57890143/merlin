package dragoman

import (
	"strings"
	"testing"
)

// Every line here is one the model would be paid to say "English" to.
var english = []string{
	"hey", "lol", "wat", "wat is this", "lmaooo no way bro", "gg ez", "ngl that was kinda mid",
	"yo who's on tonight", "im so tired of this game fr fr", "bruh moment", "ok", "idk man", "brb 5 min",
	"what are you even talking about", "the die was cast", "that was a war in a hat", "sick as hell",
	"can you send me the link", "https://example.com/niet/het/een", "`het een niet`", "```\nhet een niet ik\n```",
	"<@123456789012345678> <:niet:123456789012345678> nice", "los angeles is hot as hell", "las vegas in july? no thanks",
	"el paso or los alamos", "I met Van and Ben in Den Haag", "du jour", "c'est la vie", "que sera sera",
	"voila", "deja vu", "ja rule", "schadenfreude lol", "mañana", "jalapeño", "café", "naïve résumé",
	"hasta la vista baby", "gracias amigo", "dankjewel", "danke schön", "merci beaucoup", "arigato", "sayonara",
	"AAAAAAAAAAAA", "asdfghjkl", "hahahaha", "????", ":)", "¯\\_(ツ)_/¯", "ಠ_ಠ", "(ノಠ益ಠ)ノ彡┻━┻", "ʕ•ᴥ•ʔ", "ㅋㅋㅋㅋ", "πr²", "ツ",
	"𝓱𝓮𝓵𝓵𝓸 𝓽𝓱𝓮𝓻𝓮", "nіgger", "1 2 3", "", "o7", "te amo", "si", "no", "I'd say so", "don't", "Pokémon Go",
	"my wife says ja to everything", "he said nein and left", "mon ami", "is it on or off", "me me me", "so so so",
	"Und that is why", "over 9000", "met him at the gala", "sal and cum jokes are bad", "dan is bad at this", "do or die",
	"my mate said he's coming to the pub later", "tf you mean", "wdym tho", "stfu", "nah u", "ion even know", "finna go",
	"that movie was sehr gut", "o que?", "hoe", "hoes mad", "these hoes ain't loyal", "gaat? what's gaat",
}

var notEnglish = []string{
	"het is echt niet normaal", "ik ga morgen naar de winkel", "waarom doe je dat", "dat klopt niet", "ja maar hoe dan",
	"ich weiß nicht was du meinst", "das ist mir egal", "je ne sais pas pourquoi", "c'est pas moi qui ai fait ça",
	"no sé qué hacer ahora", "pero eso es muy caro", "eu não sei o que você quer", "non so che cosa fare", "nie wiem co to jest",
	"jag vet inte vad det är", "jeg ved ikke hvad det er", "en tiedä mitä se on, mutta joo", "bu ne demek, çok güzel bir şey",
	"aku tidak tahu apa itu", "hindi ko alam kung bakit siya galit", "nu știu ce este asta", "nem tudom hogy mi ez az", "nevím proč jsem tady, ale už není",
	"không biết là gì", "ek weet nie wat dit is nie", "nuk e di çfarë është", "Я не знаю", "что это такое", "δεν ξέρω", "لا أعرف", "אני לא יודע",
	"我不知道", "わかりません", "そうですね", "몰라요", "मुझे नहीं पता", "ฉันไม่รู้", "არ ვიცი",
	"I told him het is niet goed", "KAN JE DAT ECHT NIET",
}

func TestEnglishCostsNothing(t *testing.T) {
	for _, s := range english {
		if candidate(s, "") {
			t.Errorf("%q would be sent to a model", s)
		}
	}
}

func TestOtherLanguagesAreNoticed(t *testing.T) {
	for _, s := range notEnglish {
		if !candidate(s, "") {
			t.Errorf("%q would be missed", s)
		}
	}
}

// A short answer needs a conversation in that language behind it.
func TestAMemberMidConversationNeedsLess(t *testing.T) {
	for _, s := range []string{"ja klopt", "nee", "echt?", "lekker"} {
		if candidate(s, "") && s != "ja klopt" {
			t.Errorf("%q is enough on its own", s)
		}
		if !candidate(s, "nl") {
			t.Errorf("%q is missed mid conversation", s)
		}
	}
	for _, s := range []string{"lol", "yeah what about it", "nah the niet guy is here and that's it"} {
		if candidate(s, "nl") {
			t.Errorf("%q is English whatever came before it", s)
		}
	}
}

func TestTheWordTablesDoNotOverlap(t *testing.T) {
	for w := range englishWords {
		if langs := functionWords[w]; langs != nil {
			t.Errorf("%q is listed as English and as %v", w, langs)
		}
	}
	for w := range functionWords {
		if w != strings.ToLower(w) {
			t.Errorf("%q is never matched", w)
		}
	}
}

func BenchmarkCandidateEnglish(b *testing.B) {
	for b.Loop() {
		candidate("ngl i think the people who built this really don't know what they want", "")
	}
}
