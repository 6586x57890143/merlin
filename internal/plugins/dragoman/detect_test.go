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
	"can you send me the link", "https://example.com/niet/het/een", "```go\nhet een niet ik\n```",
	"<@123456789012345678> <:niet:123456789012345678> nice", "los angeles is hot as hell", "las vegas in july? no thanks",
	"El Paso or Los Alamos", "I met Van and Ben in Den Haag", "du jour", "c'est la vie", "que sera sera",
	"voila", "deja vu", "ja rule", "schadenfreude lol", "mañana", "jalapeño", "café", "naïve résumé",
	"hasta la vista baby", "gracias amigo", "dankjewel", "danke schön", "merci beaucoup", "arigato", "sayonara",
	"AAAAAAAAAAAA", "asdfghjkl", "hahahaha", "????", ":)", "¯\\_(ツ)_/¯", "ಠ_ಠ", "(ノಠ益ಠ)ノ彡┻━┻", "ʕ•ᴥ•ʔ", "ㅋㅋㅋㅋ", "πr²", "ツ",
	"𝓱𝓮𝓵𝓵𝓸 𝓽𝓱𝓮𝓻𝓮", "nіgger", "1 2 3", "", "o7", "te amo", "si", "no", "I'd say so", "don't", "Pokémon Go",
	"my wife says ja to everything", "he said nein and left", "mon ami", "is it on or off", "me me me", "so so so",
	"Und that is why", "over 9000", "met him at the gala", "sal and cum jokes are bad", "dan is bad at this", "do or die",
	"my mate said he's coming to the pub later", "tf you mean", "wdym tho", "stfu", "nah u", "ion even know", "finna go",
	"that movie was sehr gut",
	"skibidi gyatt rizz lmao", "sooooo goooood omggggg", "y'all trippin fr", "aint nobody got time fr dat", "Ye idk anything about what ur talking about",
	"yo lemme hop on val rq", "bro got cooked lmfaooo", "he a lil goofy ngl", "wsg gang", "gm chat", "gn fam love yall", "bet", "say less",
	"my gpu is cooked the fps drops r insane", "whos tryna run ranked", "Shrek is kinda hot tbf", "Tell Darius and Alexandra that Nurock is here",
	"LLMs are probably pretty bad at AAVE", "ts pmo sm", "type shi", "deadass bouta crash out", "bro thinks he's Kendrick", "I'd rather rewatch Evangelion",
	"playing Elden Ring w Jojo", "NO WAY THAT JUST HAPPENED", "WTF IS THIS SHIT", "lmao the bot translated that wrong", "wassup homies how yall doin",
	"gracias amigo lol", "danke schön bro", "bon appétit everyone", "hello?? hitler?? heil??", "NHH", "Yeah you do", "la la la", "la vida loca", "por favor", "ja ja ja", "mamma mia", "da baby", "das crazy", "yea das me", "er no", "om", "W take",
	"o que?", "hoe", "hoes mad", "these hoes ain't loyal", "gaat? what's gaat",
}

var notEnglish = []string{
	"het is echt niet normaal", "`gaat hij in t engels reageren. L reply`", "```\nik ga morgen naar de winkel\n```", "ik ga morgen naar de winkel", "waarom doe je dat", "dat klopt niet", "ja maar hoe dan",
	"ich weiß nicht was du meinst", "das ist mir egal", "je ne sais pas pourquoi", "c'est pas moi qui ai fait ça",
	"no sé qué hacer ahora", "pero eso es muy caro", "eu não sei o que você quer", "non so che cosa fare", "nie wiem co to jest",
	"jag vet inte vad det är", "jeg ved ikke hvad det er", "en tiedä mitä se on, mutta joo", "bu ne demek, çok güzel bir şey",
	"aku tidak tahu apa itu", "hindi ko alam kung bakit siya galit", "nu știu ce este asta", "nem tudom hogy mi ez az", "nevím proč jsem tady, ale už není",
	"không biết là gì", "ek weet nie wat dit is nie", "nuk e di çfarë është", "Я не знаю", "что это такое", "δεν ξέρω", "لا أعرف", "אני לא יודע",
	"我不知道", "わかりません", "そうですね", "몰라요", "मुझे नहीं पता", "ฉันไม่รู้", "არ ვიცი",
	"I told him het is niet goed",
	// No small words of any language: what the first version missed.
	"Nederlanders houden van nogal vreemde seks.", "LLM is waarschijnlijk behoorlijk slecht in AAVE.", "kunnen jullie alsjeblieft stil blijven",
	"morgen gezellig samen eten", "Lege winkels vandaag", "Deutsche Leute trinken gerne viel Bier", "demain nous partirons ensemble", "mañana vamos juntos",
	"domani partiamo insieme", "amanhã vamos juntos", "jutro jedziemy razem", "imorgon reser vi tillsammans", "huomenna matkustamme yhdessä",
	"yarın birlikte gidiyoruz", "besok kita pergi bersama", "bukas aalis tayo", "mâine plecăm împreună", "holnap együtt megyünk", "zítra jedeme spolu",
	"sutra idemo zajedno", "ngày mai chúng ta đi cùng nhau", "nesër shkojmë bashkë", "kesho tutaenda pamoja",
	"KAN JE DAT ECHT NIET",
	// Short ones, which the second version missed.
	"jeg er kristian", "JEG ER KLAR", "er du ok", "hvad laver du", "det er sjovt", "j'ai fait caca", "mon pantalon est sale", "du hast recht",
	"por que", "kom hier heen", "wie geht's", "vad gör du", "gdzie jesteś", "sen nerede", "ano yan", "sker der", "sker der man", "sker der snorre",
}

func TestEnglishCostsNothing(t *testing.T) {
	for _, s := range english {
		if gauge(s, "") != none {
			t.Errorf("%q would be sent to a model", s)
		}
	}
}

func TestOtherLanguagesAreNoticed(t *testing.T) {
	for _, s := range notEnglish {
		if gauge(s, "") == none {
			t.Errorf("%q would be missed", s)
		}
	}
}

// What nothing further down may skip or believe one no about.
func TestPlainForeignIsStrong(t *testing.T) {
	for _, s := range []string{"ik heb in mijn broek geplast", "j'ai fais caca dans mon pantalon", "jeg er kristian", "JEG ER KLAR"} {
		if gauge(s, "") != strong {
			t.Errorf("%q is %d", s, gauge(s, ""))
		}
	}
}

// A reply leans the way of what it answers; English and nothing lean nowhere.
func TestAReplyLeansLikeWhatItAnswers(t *testing.T) {
	for text, want := range map[string]string{
		"`gaat hij in t engels reageren. L reply`": "nl", "ich weiß nicht was du meinst": "de",
		"lol same": "", "": "",
	} {
		if got := leaning(text); got != want {
			t.Errorf("leaning(%q) = %q, want %q", text, got, want)
		}
	}
}

// A short answer needs a conversation in that language behind it.
func TestAMemberMidConversationNeedsLess(t *testing.T) {
	for _, s := range []string{"ja klopt", "nee", "echt?", "lekker", "Zwartjoekel"} {
		if gauge(s, "") != none && s != "ja klopt" {
			t.Errorf("%q is enough on its own", s)
		}
		if gauge(s, "nl") == none {
			t.Errorf("%q is missed mid conversation", s)
		}
	}
	for _, s := range []string{"lol", "yeah what about it", "nah the niet guy is here and that's it"} {
		if gauge(s, "nl") != none {
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
		gauge("ngl i think the people who built this really don't know what they want", "")
	}
}
