package audit

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/6586x57890143/cygnet/ledger"

	"github.com/6586x57890143/merlin/internal/core"
)

// bird is merlin's name on every entry and every card.
const bird = "merlin"

// toEntry is one Record call as a ledger entry: the shape every bird's log
// shares, rendered by ledger.Render.
//
// The call sites still describe what happened as strings (`user=<@1>
// duration=10m reason="x"`), so this is the one place that turns them into
// fields. Mentions become subjects, labelled by their key when the key says
// what they are to the entry ("kept", "restored"), since a mention in the
// card's grid shows as a raw ID. reason= becomes the entry's reason. Every
// other pair is a grid line, and a value with no pairs at all is the change
// from old to new. A call site that wants better than this writes the entry
// itself: this is how the 55 existing ones read well without each being
// rewritten first.
func toEntry(guildID, actorID, action, oldValue, newValue string, at time.Time) ledger.Entry {
	m := metaFor(action)
	e := ledger.Entry{
		Bird: bird, Guild: guildID, At: at, Action: entryAction(action),
		Actor: ledger.Member(actorID), Mood: moodOf(m), Summary: strings.ToLower(m.title),
	}
	if actorID == core.ActorSystem || actorID == "" {
		e.Actor = ledger.Auto()
	}
	// Message links first: their words contain spaces and brackets, which
	// parseDetail would split into pieces.
	e.Subjects = takeMessages(&newValue)
	if items, ok := parseDetail(newValue); ok && oldValue == "" {
		for _, it := range items {
			addItem(&e, it.key, it.val)
		}
		return e
	}
	oldText, oldRefs := takeMentions(oldValue)
	newText, newRefs := takeMentions(newValue)
	if len(newRefs) > 0 {
		// Mentions on both sides are a move, and the old ones are what it
		// was; on the old side alone they are just what the entry is about.
		oldRefs = labelled(oldRefs, "was")
	}
	e.Subjects = append(append(e.Subjects, oldRefs...), newRefs...)
	switch {
	case oldText != "" && newText != "":
		e.Changes = append(e.Changes, ledger.Change{Field: "value", From: gridText(oldText), To: gridText(newText)})
	case newText != "" && !strings.Contains(e.Summary, strings.ToLower(gridText(newText))):
		// (A detail that only repeats the summary says nothing twice.)
		e.Facts = append(e.Facts, ledger.Fact{Label: "detail", Value: gridText(newText)})
	case oldText != "":
		e.Facts = append(e.Facts, ledger.Fact{Label: "removed", Value: gridText(oldText)})
	}
	return e
}

// addItem files one piece of a key=value detail.
func addItem(e *ledger.Entry, key, val string) {
	if key == "reason" {
		e.Reason = val
		return
	}
	text, refs := takeMentions(val)
	if key == "" {
		// Prose between the pairs: it says what happened, so it goes with
		// the summary rather than under a made up label.
		e.Subjects = append(e.Subjects, refs...)
		if text != "" {
			e.Summary += "; " + strings.ToLower(text)
		}
		return
	}
	label := gridLabel(key)
	if key == "user" || key == "users" || key == "channel" || key == "role" || key == "target" {
		label = "" // the entry's who and where, read without a label
	}
	e.Subjects = append(e.Subjects, labelled(refs, label)...)
	if text != "" {
		e.Facts = append(e.Facts, ledger.Fact{Label: gridLabel(key), Value: gridText(text)})
	}
}

var (
	mentionRef = regexp.MustCompile(`<(@&|@!?|#)(\d+)>`)
	// messageRef is a core.MessageLink inside a masked link, which is how
	// aimod says what became of a message ("[Deleted](...)").
	messageRef = regexp.MustCompile(`\[([^\]]*)\]\(https://discord\.com/channels/\d+/(\d+)/(\d+)\)`)
)

// takeMessages takes the message links out of *v. A link's own words become
// its label ("deleted", "reposted"), since they say what the message is to
// the entry.
func takeMessages(v *string) []ledger.Ref {
	var refs []ledger.Ref
	for _, m := range messageRef.FindAllStringSubmatch(*v, -1) {
		label := strings.ToLower(m[1])
		if label == "jump to message" {
			label = ""
		}
		refs = append(refs, ledger.Message(m[2], m[3]).As(label))
	}
	*v = messageRef.ReplaceAllString(*v, "")
	return refs
}

// takeMentions splits the mentions out of v, returning what is left once
// they and the list punctuation around them are gone.
func takeMentions(v string) (string, []ledger.Ref) {
	var refs []ledger.Ref
	for _, m := range mentionRef.FindAllStringSubmatch(v, -1) {
		switch m[1] {
		case "@&":
			refs = append(refs, ledger.Role(m[2]))
		case "#":
			refs = append(refs, ledger.Channel(m[2]))
		default:
			refs = append(refs, ledger.User(m[2]))
		}
	}
	rest := mentionRef.ReplaceAllString(v, "")
	if strings.Trim(rest, "[](), →") == "" {
		rest = ""
	}
	return strings.TrimSpace(rest), refs
}

func labelled(refs []ledger.Ref, label string) []ledger.Ref {
	for i := range refs {
		refs[i].Label = label
	}
	return refs
}

// gridLabels are the keys whose label would not fit the grid's label
// column or would not say much.
var gridLabels = map[string]string{
	"unmanageable_roles": "kept",
	"linked_to":          "linked to",
	"withheld":           "withheld",
	"restored":           "restored",
	"duration":           "for",
	"release_at":         "until",
}

// gridLabel is key as a grid label: lowercase words, no wider than the
// ledger's label column.
func gridLabel(key string) string {
	if l, ok := gridLabels[key]; ok {
		return l
	}
	l := strings.ToLower(strings.ReplaceAll(key, "_", " "))
	if utf8.RuneCountInString(l) > 10 {
		l = string([]rune(l)[:10])
	}
	return strings.TrimSpace(l)
}

// gridText is v safe inside a code block: one line, no backticks, a time
// as a reader writes one, mentions
// already taken out by the caller.
func gridText(v string) string {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return strings.ToLower(t.UTC().Format("2 Jan 15:04")) + " utc"
	}
	v = strings.ReplaceAll(v, "`", "'")
	return strings.Join(strings.Fields(v), " ")
}

// entryAction is action as the ledger's <module>.<verb>. Every merlin
// action already has that shape; one without a dot gets a module so a
// card head still names something.
func entryAction(action string) string {
	action = strings.ToLower(action)
	if mod, verb, ok := strings.Cut(action, "."); ok && mod != "" && verb != "" {
		return action
	}
	return "audit." + strings.Trim(action, ".")
}

// moodOf maps the presentation table's colour onto the ledger's moods. The
// table's error colour is only ever on what cannot be undone (a deleted
// archive, a removed message, a ban), which is danger, not a failure.
func moodOf(m meta) ledger.Mood {
	if m.mood == core.MoodIdle {
		return ledger.MoodIdle
	}
	switch m.color {
	case core.ColorError:
		return ledger.MoodDanger
	case core.ColorWarning:
		return ledger.MoodWarn
	case core.ColorSuccess:
		return ledger.MoodOK
	}
	return ledger.MoodInfo
}

// moduleIcons are the action modules that are not a plugin's own name.
var moduleIcons = map[string]string{"archive": "rotation", "channel": "rotation", "config": "adminconfig"}

// brand is how merlin's cards look: her name, each plugin's icon, her
// palette, her face for the mood and her rule, which keeps every card the
// one width the embeds were.
var brand = ledger.Brand{
	Name: bird,
	Emoji: func(module string) string {
		if p, ok := moduleIcons[module]; ok {
			module = p
		}
		return core.PluginEmoji(module)
	},
	Color: func(m ledger.Mood) int {
		switch m {
		case ledger.MoodDanger, ledger.MoodError:
			return core.ColorError
		case ledger.MoodWarn:
			return core.ColorWarning
		case ledger.MoodOK:
			return core.ColorSuccess
		case ledger.MoodIdle:
			return core.ColorPrimary
		}
		return core.ColorInfo
	},
	Thumbnail: func(m ledger.Mood) string { return core.MoodURL(faces[m]) },
	Rule:      core.RuleURL,
}

// faces is which drawing of merlin each ledger mood wears, the same six the
// embeds carried: danger wears the error face, as the error colour always
// did.
var faces = map[ledger.Mood]core.Mood{
	ledger.MoodOK: core.MoodOK, ledger.MoodInfo: core.MoodInfo, ledger.MoodWarn: core.MoodWarn,
	ledger.MoodError: core.MoodError, ledger.MoodDanger: core.MoodError, ledger.MoodIdle: core.MoodIdle,
}
