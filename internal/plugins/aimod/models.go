package aimod

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Picking models, and being able to see what they cost before picking them.
//
// The reason this is a whole file rather than two setters: somebody is
// spending their own money on this, per message, forever. Handing them a
// list of model IDs and no prices means the first real feedback they get is
// a bill, and the second is a budget that ran out at lunchtime. Every
// command here answers "what will this cost me" with the two halves that
// question needs: OpenRouter's current price, and this server's own measured
// traffic.

// modelCacheTTL is how long the /models catalogue is reused.
//
// The list is several hundred entries and changes on OpenRouter's schedule,
// not this server's. Caching it is mostly about autocomplete: Discord fires
// that on nearly every keystroke, and an uncached implementation would make
// a network round trip per character typed.
const modelCacheTTL = 30 * time.Minute

// modelCache holds one guild's last /models fetch. Per guild rather than per
// process because the key is per guild, and which models a key can reach is
// a property of the account behind it.
type modelCache struct {
	mu      sync.Mutex
	entries map[string]modelCacheEntry
}

type modelCacheEntry struct {
	models    []Model
	fetchedAt time.Time
}

func newModelCache() *modelCache {
	return &modelCache{entries: make(map[string]modelCacheEntry)}
}

func (c *modelCache) get(guildID string, now time.Time) ([]Model, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[guildID]
	if !ok || now.Sub(e.fetchedAt) > modelCacheTTL {
		return nil, false
	}
	return e.models, true
}

// forgetGuild drops a guild's cached catalogue, for when the bot leaves it
// or its key changes: which models are reachable is a property of the
// account behind the key.
func (c *modelCache) forgetGuild(guildID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, guildID)
}

func (c *modelCache) put(guildID string, models []Model, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[guildID] = modelCacheEntry{models: models, fetchedAt: now}
}

// catalogue resolves the model list for a guild, from cache where possible.
func (p *Plugin) catalogue(ctx context.Context, guildID string) ([]Model, error) {
	if models, ok := p.models.get(guildID, p.now()); ok {
		return models, nil
	}
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if len(cfg.APIKeySealed) == 0 {
		return nil, fmt.Errorf("aimod: no API key configured, so model prices cannot be looked up")
	}
	key, err := p.sealer.open(cfg.APIKeySealed)
	if err != nil {
		return nil, err
	}
	client, ok := p.client.(*Client)
	if !ok {
		return nil, fmt.Errorf("aimod: model prices are unavailable in this build")
	}
	models, err := client.Models(ctx, key)
	if err != nil {
		return nil, err
	}
	p.models.put(guildID, models, p.now())
	return models, nil
}

// keyInfo asks OpenRouter about the key itself.
//
// The comma-ok assertion matters: p.client is the classifier seam, which the
// tests replace with a fake and which a future wrapper could replace with
// anything. A bare p.client.(*Client) panics inside a command handler the
// moment that happens, and the router's recover() would turn a status
// command into "the application did not respond" with no clue why.
func (p *Plugin) keyInfo(ctx context.Context, apiKey string) (KeyInfo, error) {
	client, ok := p.client.(*Client)
	if !ok {
		return KeyInfo{}, fmt.Errorf("aimod: OpenRouter account details are unavailable in this build")
	}
	return client.KeyInfo(ctx, apiKey)
}

// reasoningLine says whether this stack is paying for the model to think.
//
// Shown next to the price because the two multiply: reasoning tokens are
// billed at the completion rate, so a stack that cannot have it switched off
// costs more per message than its headline price suggests, and an admin
// comparing models should be able to see which they are looking at.
//
// The answer is only known after a stack has been used once, since it is
// learned from the endpoint rejecting the attempt rather than read from
// metadata. Before that it reports the intent, which is what will be tried.
func (p *Plugin) reasoningLine(models []string) string {
	client, ok := p.client.(*Client)
	if !ok {
		return ""
	}
	if client.ReasoningDisabled(models) {
		return "_Reasoning switched off: you pay for the answer, not for the thinking._"
	}
	return "_This endpoint requires reasoning, so thinking tokens are billed on every message it reads._"
}

func findModel(models []Model, id string) (Model, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// priceLine renders one model for a human. The prices are per million
// tokens because that is the unit OpenRouter publishes and the unit every
// comparison anybody has already seen is in.
func priceLine(id string, m Model, found bool) string {
	if !found {
		return fmt.Sprintf("`%s` - price unknown (not in this key's catalogue)", id)
	}
	if m.Free {
		return fmt.Sprintf("`%s` - free (capped at 20 requests/min and 50-1000/day account-wide)", id)
	}
	return fmt.Sprintf("`%s` - $%.3f in / $%.3f out per million tokens", id, m.PromptPerM, m.CompletionPerM)
}

func (p *Plugin) handleModelsShow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Deferred: this makes a network call to OpenRouter on a cold cache, and
	// Discord's three second budget is not enough to gamble on somebody
	// else's API being quick. spec.MD section 4a.
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer models show", "guild", i.GuildID, "err", err)
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}
	fastIDs := modelsOr(cfg.FastModels, defaultFastModels)
	deepIDs := modelsOr(cfg.DeepModels, defaultDeepModels)

	var body strings.Builder
	body.WriteString("Every message that gets past the free pattern checks is read by the **cheap pass**. " +
		"Only what that flags is re-read by the **deep pass**, which is the only one that can delete anything. " +
		"That is why the two are priced so differently and configured separately.\n\n")

	catalogue, cerr := p.catalogue(ctx, i.GuildID)
	color := core.ColorInfo
	if cerr != nil {
		color = core.ColorWarning
		body.WriteString("Live prices could not be fetched (" + cerr.Error() + "), so only the configured models are listed.\n\n")
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Cheap pass, reads everything", Value: core.TruncateEmbedField(
			stackLines(fastIDs, catalogue, cfg.FastModels) + "\n" + p.reasoningLine(fastIDs))},
		{Name: "Deep pass, confirms before acting", Value: core.TruncateEmbedField(
			stackLines(deepIDs, catalogue, cfg.DeepModels) + "\n" + p.reasoningLine(deepIDs))},
	}

	// The projection. Measured traffic where there is any, clearly labelled
	// assumptions where there is not, and never a bare number with no
	// provenance: an admin is about to set a budget from this.
	history, err := p.store.SpendSince(ctx, i.GuildID, today(p.now()).AddDate(0, 0, -7))
	if err != nil {
		p.log.Error("aimod: read spend history", "guild", i.GuildID, "err", err)
	}
	if override := core.LeafArgs(i)["messages_per_day"]; override != nil {
		// An explicit volume replaces the measured one but keeps the
		// measured tokens-per-message, which is the half that is genuinely
		// this server's own and the half an admin cannot guess.
		history = scaleHistory(history, float64(override.IntValue()))
	}

	fastModel, _ := findModel(catalogue, fastIDs[0])
	deepModel, _ := findModel(catalogue, deepIDs[0])
	est := estimateFor(history, fastModel, deepModel)

	// What the current stack costs comes from the receipts, not from a price
	// list. OpenRouter returns the cost of every call, so for a stack the
	// guild is already running there is nothing to estimate, and estimating
	// anyway was reporting six times under what the account had been charged:
	// a model missing from the catalogue silently priced at zero and dragged
	// the whole figure down. The projection is kept for the case it is
	// actually needed, which is a guild that has not spent anything yet.
	var costLine string
	if est.ActualPerDay > 0 {
		costLine = fmt.Sprintf("**%s per day**, about %s a month\nActually billed, %s",
			formatUSD(est.ActualPerDay), formatUSD(est.ActualPerDay*30), est.Basis)
	} else {
		costLine = fmt.Sprintf("**%s per day** projected, about %s a month\nBased on %s",
			formatUSD(est.USDPerDay), formatUSD(est.USDPerDay*30), est.Basis)
	}
	costLine += fmt.Sprintf("\n%.0f messages/day reaching a model, %.0f tokens each, %.1f%% of them escalating to the deep pass",
		est.ScannedPerDay, est.FastTokensPerMsg, est.DeepRate*100)
	if len(est.Unpriced) > 0 {
		costLine += "\nNo price listed for " + strings.Join(est.Unpriced, ", ") +
			", so any projection is a floor rather than a figure."
	}
	fields = append(fields, &discordgo.MessageEmbedField{
		Name:  "Cost",
		Value: core.TruncateEmbedField(costLine),
	})

	var spentWeek float64
	for _, sp := range history {
		spentWeek += sp.SpentUSD
	}
	fields = append(fields,
		&discordgo.MessageEmbedField{Name: "Actually spent, last 7 days", Value: formatUSD(spentWeek), Inline: true},
		&discordgo.MessageEmbedField{Name: "Daily budget", Value: formatUSD(cfg.DailyBudgetUSD), Inline: true},
	)

	body.WriteString("`/aimod models compare <model>` prices any other model against this same traffic before you switch to it.")

	embed := core.NewEmbed(color, "Models and cost", core.TruncateEmbedDescription(body.String()), fields...)
	if err := core.FollowUpEmbed(s, i, embed); err != nil {
		p.log.Error("aimod: respond models show", "guild", i.GuildID, "err", err)
	}
}

// stackLines renders one configured stack, saying plainly when it is the
// compiled-in default rather than something the guild chose. That
// distinction is worth the line: a default tracks whatever this bot ships
// next, and a pinned list does not.
func stackLines(ids []string, catalogue []Model, configured []string) string {
	var b strings.Builder
	for n, id := range ids {
		m, found := findModel(catalogue, id)
		prefix := "first choice"
		if n > 0 {
			prefix = "fallback"
		}
		fmt.Fprintf(&b, "%s: %s\n", prefix, priceLine(id, m, found))
	}
	if len(configured) == 0 {
		b.WriteString("_Merlin's defaults, which track whatever this bot ships. Set your own with the commands below._")
	}
	return b.String()
}

// scaleHistory rewrites measured history to a stated daily volume, keeping
// the measured cost shape (tokens per message, escalation rate) intact.
func scaleHistory(history []Spend, messagesPerDay float64) []Spend {
	if messagesPerDay <= 0 || len(history) == 0 {
		return history
	}
	var scanned float64
	for _, sp := range history {
		scanned += float64(sp.Scanned)
	}
	if scanned == 0 {
		return history
	}
	factor := (messagesPerDay * float64(len(history))) / scanned
	out := make([]Spend, 0, len(history))
	for _, sp := range history {
		sp.Scanned = int(float64(sp.Scanned) * factor)
		sp.FastPromptTokens = int64(float64(sp.FastPromptTokens) * factor)
		sp.FastCompletionTokens = int64(float64(sp.FastCompletionTokens) * factor)
		sp.DeepPromptTokens = int64(float64(sp.DeepPromptTokens) * factor)
		sp.DeepCompletionTokens = int64(float64(sp.DeepCompletionTokens) * factor)
		sp.DeepCalls = int(float64(sp.DeepCalls) * factor)
		out = append(out, sp)
	}
	return out
}

func (p *Plugin) handleModelsCompare(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer models compare", "guild", i.GuildID, "err", err)
		return
	}
	opts := core.LeafArgs(i)
	id := strings.TrimSpace(opts["model"].Value.(string))
	pass := "fast"
	if v, ok := opts["pass"]; ok {
		pass = v.Value.(string)
	}

	catalogue, err := p.catalogue(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Could not fetch model prices", err)
		return
	}
	candidateModel, found := findModel(catalogue, id)
	if !found {
		_ = core.FollowUpErr(s, i, "Unknown model", fmt.Errorf("%q is not in this key's catalogue", id))
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}
	history, err := p.store.SpendSince(ctx, i.GuildID, today(p.now()).AddDate(0, 0, -7))
	if err != nil {
		p.log.Error("aimod: read spend history", "guild", i.GuildID, "err", err)
	}

	currentFast, _ := findModel(catalogue, modelsOr(cfg.FastModels, defaultFastModels)[0])
	currentDeep, _ := findModel(catalogue, modelsOr(cfg.DeepModels, defaultDeepModels)[0])
	now := estimateFor(history, currentFast, currentDeep)

	after := now
	if pass == "deep" {
		after = estimateFor(history, currentFast, candidateModel)
	} else {
		after = estimateFor(history, candidateModel, currentDeep)
	}

	delta := after.USDPerDay - now.USDPerDay
	direction := "more"
	if delta < 0 {
		direction = "less"
		delta = -delta
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Model", Value: priceLine(id, candidateModel, true)},
		{Name: "As your " + pass + " pass", Value: formatUSD(after.USDPerDay) + " per day", Inline: true},
		{Name: "Currently", Value: formatUSD(now.USDPerDay) + " per day", Inline: true},
		{Name: "Difference", Value: formatUSD(delta) + " per day " + direction, Inline: true},
		{Name: "Based on", Value: after.Basis},
	}
	if pass == "fast" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Worth remembering",
			Value: "The cheap pass can only ever flag. Nothing it says gets a message deleted without the deep pass " +
				"confirming it, so this is the one to optimise for price.",
		})
	} else {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Worth remembering",
			Value: "The deep pass is the only thing that can delete a message. It reads about one message in a hundred, " +
				"so a better model here costs little and is where accuracy actually comes from.",
		})
	}

	embed := core.NewEmbed(core.ColorInfo, "Cost of switching", "", fields...)
	if err := core.FollowUpEmbed(s, i, embed); err != nil {
		p.log.Error("aimod: respond models compare", "guild", i.GuildID, "err", err)
	}
}

func (p *Plugin) handleSetFast(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	p.setStack(ctx, s, i, true)
}

func (p *Plugin) handleSetDeep(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	p.setStack(ctx, s, i, false)
}

// maxStackModels bounds a configured fallback chain. Past a handful, the
// later entries are only ever reached when everything before them is down at
// once, and a long chain mostly serves to hide that the first entry is
// broken.
const maxStackModels = 5

func (p *Plugin) setStack(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, fast bool) {
	raw := strings.TrimSpace(core.LeafArgs(i)["models"].Value.(string))
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}

	var ids []string
	// "default" clears the pin rather than storing today's defaults as a
	// literal list, so a guild that wants to track this bot's choices keeps
	// tracking them across upgrades.
	if !strings.EqualFold(raw, "default") {
		for _, part := range strings.Split(raw, ",") {
			if id := strings.TrimSpace(part); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			core.RespondErr(s, i, "No models given",
				fmt.Errorf("give one or more model IDs separated by commas, or \"default\""))
			return
		}
		if len(ids) > maxStackModels {
			core.RespondErr(s, i, "Too many models", fmt.Errorf("at most %d, in order of preference", maxStackModels))
			return
		}
	}

	nextFast, nextDeep := cfg.FastModels, cfg.DeepModels
	if fast {
		nextFast = ids
	} else {
		nextDeep = ids
	}
	if err := p.store.SetModels(ctx, i.GuildID, nextFast, nextDeep); err != nil {
		core.RespondErr(s, i, "Failed to set the models", err)
		return
	}

	which := "deep"
	if fast {
		which = "fast"
	}
	p.auditConfig(ctx, i, "aimod.models_set", which, strings.Join(ids, ", "))

	if len(ids) == 0 {
		core.RespondOK(s, i, "Tracking the defaults",
			fmt.Sprintf("The %s pass will use Merlin's own defaults, and will follow them if they change.\n\n"+
				"`/aimod models show` for the current prices.", which))
		return
	}
	core.RespondOK(s, i, "Models set",
		fmt.Sprintf("The %s pass will try, in order:\n%s\n\nLater entries are only used when an earlier one errors "+
			"or is rate limited.\n\n`/aimod models show` to see what that costs at this server's traffic.",
			which, "- "+strings.Join(ids, "\n- ")))
}

// autocompleteModel suggests model IDs with their prices attached.
//
// The price is in the label because this is the moment somebody is choosing,
// and a list of opaque IDs at this moment is how a guild ends up running its
// every-message pass on a frontier model. Discord rejects a response with
// more than 25 choices outright (spec.MD section 4a), so the list is sorted
// cheapest first and cut there: if it has to be truncated, the cheap end is
// the half worth keeping.
func (p *Plugin) autocompleteModel(ctx context.Context, i *discordgo.InteractionCreate, _, focusedValue string) []*discordgo.ApplicationCommandOptionChoice {
	catalogue, err := p.catalogue(ctx, i.GuildID)
	if err != nil {
		return nil
	}

	// Only the last comma-separated entry is being typed; everything before
	// it is already chosen and is preserved in the suggestion, so completing
	// a fallback chain does not throw away what is already there.
	prefix := ""
	needle := focusedValue
	if idx := strings.LastIndex(focusedValue, ","); idx >= 0 {
		prefix = focusedValue[:idx+1] + " "
		needle = strings.TrimSpace(focusedValue[idx+1:])
	}
	needle = strings.ToLower(needle)

	matches := make([]Model, 0, len(catalogue))
	for _, m := range catalogue {
		if needle == "" || strings.Contains(strings.ToLower(m.ID), needle) || strings.Contains(strings.ToLower(m.Name), needle) {
			matches = append(matches, m)
		}
	}
	sort.Slice(matches, func(a, b int) bool {
		if matches[a].PromptPerM != matches[b].PromptPerM {
			return matches[a].PromptPerM < matches[b].PromptPerM
		}
		return matches[a].ID < matches[b].ID
	})

	const maxChoices = 25
	if len(matches) > maxChoices {
		matches = matches[:maxChoices]
	}
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(matches))
	for _, m := range matches {
		label := fmt.Sprintf("%s ($%.3f/$%.3f per M)", m.ID, m.PromptPerM, m.CompletionPerM)
		if m.Free {
			label = m.ID + " (free, rate limited)"
		}
		value := prefix + m.ID
		// Discord caps a choice name at 100 characters and a value at 100
		// too; a long model ID plus its price can reach that.
		if len(label) > 100 {
			label = label[:100]
		}
		if len(value) > 100 {
			continue
		}
		out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: label, Value: value})
	}
	return out
}
