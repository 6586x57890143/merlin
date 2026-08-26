package aimod

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Default model stacks.
//
// Ordered cheapest-first within each tier, and passed to OpenRouter as a
// fallback array, so an entry that is rate limited or down costs a retry
// rather than a missed scan. A guild overrides either stack with
// /aimod models set-fast|set-deep.
//
// Model IDs churn far faster than this repository does, which is why these
// are a starting point rather than a decision: check openrouter.ai/models
// before assuming any of them still exists. A stack whose entries have all
// been retired fails loudly (the API returns an error naming the model)
// rather than silently falling back to something expensive.
var (
	// The fast tier reads every scanned message. It wants to be cheap and
	// fast far more than it wants to be right: its only power is to flag,
	// and everything it flags is re-read by the deep tier before anything
	// happens to a message.
	defaultFastModels = []string{
		"google/gemini-2.5-flash-lite",
		"openai/gpt-5-nano",
	}
	// The deep tier reads roughly one message in a hundred and is the only
	// tier whose verdict can delete anything, so it is chosen for judgement
	// rather than price.
	defaultDeepModels = []string{
		"openai/gpt-5-mini",
		"anthropic/claude-haiku-4.5",
		"google/gemini-2.5-flash",
	}
)

func modelsOr(configured, fallback []string) []string {
	if len(configured) > 0 {
		return configured
	}
	return fallback
}

// Confidence thresholds.
//
// deepThreshold is how sure the fast tier has to be before its hit is worth
// paying the deep tier to re-read. Below it the hit is dropped entirely
// rather than escalated: a nano model at 20% confidence is noise, and
// escalating noise is how the cheap tier ends up driving the expensive one.
//
// actThreshold is how sure the deep tier has to be before a message is
// touched. High, deliberately. A missed violation is caught by the next
// report, by a moderator, or by the next message from the same person; a
// wrongly deleted message on a free-speech server is the failure this whole
// plugin is supposed to be worth tolerating, and it is not recoverable by
// waiting.
const (
	deepThreshold = 0.45
	actThreshold  = 0.75
)

// Token ceilings. The fast pass answers with a list of indices and a
// confidence each, which is tiny; the deep pass may also return a rewritten
// message, so it gets room for one. Capping them is not politeness, it is
// the difference between a runaway model costing cents and costing dollars.
const (
	fastMaxTokens = 400
	deepMaxTokens = 900
)

// Verdict is one message's classification.
type Verdict struct {
	Index      int     `json:"i"`
	Bucket     Bucket  `json:"b"`
	Confidence float64 `json:"c"`
}

// deepVerdict is the second pass's answer about one message.
type deepVerdict struct {
	Violation  bool    `json:"violation"`
	Bucket     Bucket  `json:"bucket"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Rewrite    string  `json:"rewrite"`
}

// candidate is one message on its way through the ladder.
type candidate struct {
	MessageID string
	ChannelID string
	AuthorID  string
	Content   string
}

// The shared framing for both passes.
//
// Two things in here are doing the real work and should not be softened.
// The first is that this is a Discord Terms of Service filter and not a
// civility filter: the servers this runs on argue, insult each other and
// hold views a general purpose model has been trained to find distasteful,
// and a model left to its own instincts flags all of it. The second is that
// the not_violations lists travel with the buckets, so the model is told
// where each rule stops in the same breath as where it starts.
const systemPreamble = `You enforce Discord's Community Guidelines, and nothing else.

You are not a civility filter. This server permits rudeness, insults, profanity, hostile argument, dark humour, offensive opinions, political and religious views of every kind, and discussion of illegal or disturbing subjects. None of that is a violation. Only report content that breaches one of the numbered policies below as those policies are written.

When a message is ambiguous, or when it could plausibly fall under one of a policy's "not a violation" lines, it is not a violation. Report nothing rather than something you are unsure about.

Judge only the message text you are given. Do not infer intent that is not there, and do not report a message for the subject it discusses rather than for what it does.

Judge what a message means, not how it is spelled. Misspellings, swapped or repeated letters, digits or symbols standing in for letters, and spaces or punctuation inserted mid-word are the ordinary ways of writing a word that would otherwise be caught, and they count as that word. A plain typo in a sentence that is not otherwise a violation is still not a violation.`

// fastPrompt builds the batch classifier's system message: one line per
// enforced bucket, and nothing else.
//
// This is where the cost of the whole feature is decided. The preamble plus
// ten one-line bucket summaries is roughly 350 tokens, amortized across
// every message in the batch, and the full policy files (several thousand
// tokens) are never sent at this tier at all.
func (p *Plugin) fastPrompt(buckets []Bucket) string {
	var b strings.Builder
	b.WriteString(systemPreamble)
	b.WriteString("\n\nPolicies:\n")
	for _, bucket := range buckets {
		pol, ok := p.policies[bucket]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", bucket, pol.Short)
	}
	b.WriteString(`
You are given numbered messages. Return JSON: {"v":[{"i":<number>,"b":"<policy>","c":<0.0-1.0>}]}
Include an entry only for a message that breaches a policy. Most batches contain none; return {"v":[]} for those. c is your confidence that this is a genuine breach as written.`)
	return b.String()
}

// fastSchema constrains the batch answer. strict, so a model that would
// otherwise wander into prose is held to the shape this code parses.
func fastSchema() *responseFormat {
	return &responseFormat{
		Type: "json_schema",
		JSONSchema: jsonSchema{
			Name:   "verdicts",
			Strict: true,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"v": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"i": map[string]any{"type": "integer"},
								"b": map[string]any{"type": "string"},
								"c": map[string]any{"type": "number"},
							},
							"required":             []string{"i", "b", "c"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"v"},
				"additionalProperties": false,
			},
		},
	}
}

// classifyFast runs rung 2 over a batch. Returns the hits, the usage (even
// on a parse failure, because the call was billed either way), and an error.
func (p *Plugin) classifyFast(ctx context.Context, apiKey string, cfg Config, batch []candidate) ([]Verdict, Usage, error) {
	buckets := enforcedBuckets(cfg)
	if len(buckets) == 0 || len(batch) == 0 {
		return nil, Usage{}, nil
	}

	var user strings.Builder
	for i, c := range batch {
		// Numbered from 1: a model that miscounts from zero produces an
		// off-by-one that silently actions the wrong person's message, and
		// one-based indexing is what the model has seen most of.
		fmt.Fprintf(&user, "%d. %s\n", i+1, sanitizeForPrompt(c.Content))
	}

	out, usage, err := p.client.Chat(ctx, apiKey, chatRequest{
		Models:   modelsOr(cfg.FastModels, defaultFastModels),
		Provider: strictProvider(),
		Messages: []chatMessage{
			{Role: "system", Content: p.fastPrompt(buckets)},
			{Role: "user", Content: user.String()},
		},
		ResponseFormat: fastSchema(),
		MaxTokens:      fastMaxTokens,
	})
	if err != nil {
		return nil, usage, err
	}

	var parsed struct {
		V []Verdict `json:"v"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, usage, fmt.Errorf("aimod: fast pass returned unparseable JSON: %w", err)
	}

	// Filter here rather than trusting the response. A model can and does
	// return an index outside the batch, a bucket that does not exist, or a
	// bucket this guild has switched off, and every one of those would
	// otherwise become an incident against the wrong message or the wrong
	// policy.
	var hits []Verdict
	for _, v := range parsed.V {
		if v.Index < 1 || v.Index > len(batch) {
			continue
		}
		if !known(v.Bucket) {
			continue
		}
		if EffectiveAction(cfg.BucketActions, v.Bucket) == ActionOff {
			continue
		}
		if v.Confidence < deepThreshold {
			continue
		}
		hits = append(hits, v)
	}
	return hits, usage, nil
}

// deepPrompt builds the second pass for one bucket. This is the only place
// the full policy file is sent, and it is sent for exactly one bucket: the
// one the fast pass named.
func (p *Plugin) deepPrompt(bucket Bucket, wantRewrite bool) string {
	pol, ok := p.policies[bucket]
	if !ok {
		return systemPreamble
	}

	var b strings.Builder
	b.WriteString(systemPreamble)
	fmt.Fprintf(&b, "\n\nA first-pass filter flagged the message below as possibly breaching one specific policy. Your job is to confirm or clear it. Most flags from that filter are wrong; clearing one is a normal outcome.\n\nPolicy: %s\n%s\n", bucket, pol.Short)

	b.WriteString("\nDefinitions:\n")
	for _, d := range pol.Definitions {
		fmt.Fprintf(&b, "- %s\n", d)
	}
	b.WriteString("\nThis IS a violation:\n")
	for _, v := range pol.Violations {
		fmt.Fprintf(&b, "- %s\n", v)
	}
	b.WriteString("\nThis is NOT a violation:\n")
	for _, v := range pol.NotViolations {
		fmt.Fprintf(&b, "- %s\n", v)
	}

	b.WriteString("\nReturn JSON with: violation (bool), bucket (the policy name), confidence (0.0-1.0), reason (one short sentence a moderator will read)")
	if wantRewrite {
		// The rewrite is asked for in the same call rather than a third one:
		// the model has already read the message and the policy, so a
		// separate call would pay for both again to produce the same answer.
		b.WriteString(", rewrite (the message with only the violating part removed or replaced, keeping the author's own wording, tone and meaning everywhere else; empty string if nothing publishable remains)")
	}
	b.WriteString(".")
	return b.String()
}

func deepSchema(wantRewrite bool) *responseFormat {
	props := map[string]any{
		"violation":  map[string]any{"type": "boolean"},
		"bucket":     map[string]any{"type": "string"},
		"confidence": map[string]any{"type": "number"},
		"reason":     map[string]any{"type": "string"},
	}
	required := []string{"violation", "bucket", "confidence", "reason"}
	if wantRewrite {
		props["rewrite"] = map[string]any{"type": "string"}
		required = append(required, "rewrite")
	}
	return &responseFormat{
		Type: "json_schema",
		JSONSchema: jsonSchema{
			Name:   "verdict",
			Strict: true,
			Schema: map[string]any{
				"type":                 "object",
				"properties":           props,
				"required":             required,
				"additionalProperties": false,
			},
		},
	}
}

// classifyDeep runs rung 3 on one message. context holds the few messages
// that preceded it in the channel, which is what makes a threat
// distinguishable from a running joke.
func (p *Plugin) classifyDeep(ctx context.Context, apiKey string, cfg Config, bucket Bucket, c candidate, priorLines []string, wantRewrite bool) (deepVerdict, Usage, error) {
	var user strings.Builder
	if len(priorLines) > 0 {
		user.WriteString("Recent messages in this channel, for context only. Do not judge these:\n")
		for _, line := range priorLines {
			fmt.Fprintf(&user, "- %s\n", sanitizeForPrompt(line))
		}
		user.WriteString("\n")
	}
	fmt.Fprintf(&user, "The message to judge:\n%s", sanitizeForPrompt(c.Content))

	out, usage, err := p.client.Chat(ctx, apiKey, chatRequest{
		Models:   modelsOr(cfg.DeepModels, defaultDeepModels),
		Provider: strictProvider(),
		Messages: []chatMessage{
			{Role: "system", Content: p.deepPrompt(bucket, wantRewrite)},
			{Role: "user", Content: user.String()},
		},
		ResponseFormat: deepSchema(wantRewrite),
		MaxTokens:      deepMaxTokens,
	})
	if err != nil {
		return deepVerdict{}, usage, err
	}

	var v deepVerdict
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return deepVerdict{}, usage, fmt.Errorf("aimod: deep pass returned unparseable JSON: %w", err)
	}
	// The bucket is pinned to the one that was asked about. A model
	// answering about a different policy than the one it was handed is
	// answering a question nobody asked, and letting it redirect would mean
	// a bucket the guild switched off could still get a message deleted.
	v.Bucket = bucket
	return v, usage, nil
}

// maxPromptChars caps one message's contribution to a prompt.
//
// Discord allows 4000 characters with Nitro, and a batch of twenty of those
// is a prompt an order of magnitude larger than the policy text around it.
// A violation that needs more than this much text to be visible is not one
// this tier was going to catch anyway.
const maxPromptChars = 1200

// sanitizeForPrompt bounds one message and flattens it onto lines that
// cannot be mistaken for the numbered structure around them.
//
// Not a security boundary, and it would be a mistake to treat it as one:
// prompt injection from message content is real and unsolvable here, which
// is precisely why the model's answer is constrained to a JSON schema, its
// bucket is overwritten with the one it was asked about, its index is
// range-checked, and nothing it says can do more than delete or rewrite the
// message it was given. A member who talks their way past the classifier
// gets their own message left alone, which is the same outcome as the
// classifier simply being wrong.
func sanitizeForPrompt(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > maxPromptChars {
		s = string([]rune(s)[:maxPromptChars]) + " [truncated]"
	}
	return strings.TrimSpace(s)
}
