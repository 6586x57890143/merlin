package aimod

import (
	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Action namespaces for the permission whitelist (core.PermSpec.Action).
//
// Four rather than one, split by blast radius, the same reasoning that keeps
// roles.jail_role separate from roles.jail. Reading is Mod. Undoing is Mod,
// because reversing a false positive has to be available to whoever is
// actually watching the channel at 3am, and its worst case is a message
// reappearing. Policy and configuration are Admin: one decides what the
// server enforces, the other holds a spending credential.
const (
	actionRead      = "aimod.read"
	actionUndo      = "aimod.undo"
	actionPolicy    = "aimod.policy"
	actionConfigure = "aimod.configure"
)

func (p *Plugin) registerCommands() {
	modeChoices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(Modes))
	for _, m := range Modes {
		modeChoices = append(modeChoices, &discordgo.ApplicationCommandOptionChoice{Name: string(m), Value: string(m)})
	}
	providerChoices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(providers))
	for _, spec := range providers {
		providerChoices = append(providerChoices, &discordgo.ApplicationCommandOptionChoice{Name: spec.label, Value: spec.name})
	}
	// A fixed Choices list rather than autocomplete, matching rotation's
	// disclosure option: these are a closed set known at compile time, and
	// spec.MD section 4a reserves autocomplete for values that come from bot
	// state and cannot be enumerated here. Model IDs, below, are the
	// opposite case and do get autocomplete.
	sanctionChoices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(SanctionActions))
	for _, a := range SanctionActions {
		sanctionChoices = append(sanctionChoices, &discordgo.ApplicationCommandOptionChoice{Name: string(a), Value: string(a)})
	}
	bucketChoices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(AllBuckets))
	for _, b := range AllBuckets {
		bucketChoices = append(bucketChoices, &discordgo.ApplicationCommandOptionChoice{Name: string(b), Value: string(b)})
	}
	actionChoices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(Actions))
	for _, a := range Actions {
		actionChoices = append(actionChoices, &discordgo.ApplicationCommandOptionChoice{Name: string(a), Value: string(a)})
	}

	messageOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "message_id",
		Description: "The message's ID (Developer Mode: right-click the message, Copy Message ID)", Required: true,
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "aimod",
		Description: "AI moderation against Discord's Community Guidelines",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "status",
				Description: "What this server is scanning, what it has spent today, and whether anything is wrong",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "why",
				Description: "Show why a message was removed, rewritten or flagged",
				Options:     []*discordgo.ApplicationCommandOption{messageOpt},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "undo",
				Description: "Reverse a false positive: repost the original message and mark the incident undone",
				Options:     []*discordgo.ApplicationCommandOption{messageOpt},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "moderate-me",
				Description: "Ask to be jailed by the automatic sanctions like anyone else, even if you are staff",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type: discordgo.ApplicationCommandOptionBoolean, Name: "enabled",
						Description: "True to opt in, false to go back to the default", Required: true,
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "moderate-user",
				Description: "Operator only: put somebody else on the automatic-sanction list",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type: discordgo.ApplicationCommandOptionUser, Name: "user",
						Description: "The member", Required: true,
					},
					{
						Type: discordgo.ApplicationCommandOptionBoolean, Name: "enabled",
						Description: "True to opt them in, false to remove them", Required: true,
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "policy",
				Description: "Which of Discord's guidelines this server enforces, and how",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "list",
						Description: "Show every policy area and what this server does about it",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "set",
						Description: "Set what happens to a message breaching one policy area",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "policy",
								Description: "The policy area", Required: true, Choices: bucketChoices,
							},
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "action",
								Description: "off, flag (record only), rewrite (clean it up) or remove",
								Required:    true, Choices: actionChoices,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "explain",
						Description: "Show exactly what one policy area covers, and what it does not",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "policy",
								Description: "The policy area", Required: true, Choices: bucketChoices,
							},
						},
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "models",
				Description: "Which models each pass uses, what they cost, and what that projects to per day",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "show",
						Description: "Current models with live prices, this server's spend, and a projected daily cost",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionInteger, Name: "messages_per_day",
								Description: "Project against this volume instead of this server's measured traffic",
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "set-fast",
						Description: "The cheap pass that reads every message. Comma separated; later entries are fallbacks.",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "models",
								Description: "Model IDs, cheapest first. Leave as \"default\" to track merlin's own defaults.",
								Required:    true, Autocomplete: true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "set-deep",
						Description: "The pass that confirms a flag before anything is deleted. Worth spending on.",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "models",
								Description: "Model IDs, best first. Leave as \"default\" to track merlin's own defaults.",
								Required:    true, Autocomplete: true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "compare",
						Description: "Price a model against this server's measured traffic before switching to it",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "model",
								Description: "The model to price", Required: true, Autocomplete: true,
							},
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "pass",
								Description: "Which pass it would run (default: fast)",
								Choices: []*discordgo.ApplicationCommandOptionChoice{
									{Name: "fast", Value: "fast"},
									{Name: "deep", Value: "deep"},
								},
							},
						},
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "configure",
				Description: "Key, mode, budget and exemptions",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "key",
						Description: "Set this server's model provider API key (stored encrypted, never shown again)",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "key",
								Description: "An OrcaRouter or OpenRouter key. Which one is read off the key itself.",
								Required:    true,
							},
							{
								// Optional, and normally unused: the prefix says which
								// gateway a key belongs to. This is the escape hatch for
								// the day a gateway changes what its keys look like, so
								// that a valid key is never refused for being unfamiliar.
								// A fixed pair of values, so Choices rather than
								// autocomplete: spec.MD 4a's autocomplete rule is about
								// values that come from bot state.
								Type: discordgo.ApplicationCommandOptionString, Name: "provider",
								Description: "Only needed if the key's prefix is not recognised",
								Choices:     providerChoices,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "mode",
						Description: "off, flag (watch what it would do), or enforce",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "mode",
								Description: "Run in flag for a week before enforcing", Required: true, Choices: modeChoices,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "budget",
						Description: "Most this server may spend on models per day, in USD",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionNumber, Name: "usd",
								Description: "Past this, scanning falls back to the free pattern checks until midnight UTC",
								Required:    true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "evidence",
						Description: "How long the original text of a moderated message is kept so it can be undone",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionInteger, Name: "hours",
								Description: "0 keeps nothing, and makes /aimod undo impossible", Required: true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "exempt-channel",
						Description: "Stop scanning a channel, or start again",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionChannel, Name: "channel",
								Description: "The channel", Required: true,
							},
							{
								Type: discordgo.ApplicationCommandOptionBoolean, Name: "exempt",
								Description: "True to stop scanning it, false to resume", Required: true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "exempt-role",
						Description: "Stop scanning messages from holders of a role, or start again",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionRole, Name: "role",
								Description: "The role", Required: true,
							},
							{
								Type: discordgo.ApplicationCommandOptionBoolean, Name: "exempt",
								Description: "True to stop scanning them, false to resume", Required: true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "sanctions",
						Description: "Whether a confirmed violation also jails the member, for a length that escalates",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "action",
								Description: "off, flag (report only) or jail", Required: true, Choices: sanctionChoices,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "triage",
						Description: "The local pre-filter that decides which messages are worth a model call",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "mode",
								Description: "off, shadow (learn only) or on", Required: true,
								Choices: triageModeChoices(),
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "show",
						Description: "Everything configured here, with the key masked",
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "funding",
				Description: "The tip jar that pays for the scanning, and how much credit is left",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "show",
						Description: "How much scanning credit is left, and where to chip in",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "set-address",
						Description: "Point the tip jar at a wallet (server owner only)",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "address",
								Description: "An EVM (0x...), TRON (T...) or Solana wallet address",
								Required:    true,
							},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "clear",
						Description: "Remove the tip jar and stop reading its balance",
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
				Name:        "calibrate",
				Description: "The weekly review that tunes the filter to how this server actually talks",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "show",
						Description: "What the filter has learned, and anything waiting to be applied",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "run-now",
						Description: "Run the review immediately instead of waiting for the weekly one",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "apply",
						Description: "Put the proposed calibration into force",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "clear",
						Description: "Forget everything learned and go back to the built-in policies alone",
					},
					{
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Name:        "mode",
						Description: "Whether the weekly review applies itself, only proposes, or never runs",
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type: discordgo.ApplicationCommandOptionString, Name: "mode",
								Description: "off, suggest (propose and wait), or auto (apply on its own)",
								Required:    true,
								// A closed set known at compile time, so fixed
								// choices rather than autocomplete: spec.MD
								// section 4a's autocomplete rule is about
								// values that come from bot state.
								Choices: calibrationModeChoices(),
							},
						},
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)

	p.commands.Handle("aimod", "status", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleStatus)
	p.commands.Handle("aimod", "why", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleWhy)
	p.commands.Handle("aimod", "undo", core.PermSpec{Tier: core.TierMod, Action: actionUndo}, p.handleUndo)

	// TierPublic: consenting to be moderated is not a privilege, and gating
	// it behind a tier would mean the members most likely to want it (staff,
	// who are the only ones it changes anything for) are also the only ones
	// who could reach it, which is backwards.
	p.commands.Handle("aimod", "moderate-me", core.PermSpec{Tier: core.TierPublic}, p.handleModerateMe)
	// TierMod is the coarse gate; the real one is the operator check inside
	// the handler, because no tier expresses "one named identity".
	p.commands.Handle("aimod", "moderate-user", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModerateUser)

	p.commands.Handle("aimod", "policy/list", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePolicyList)
	p.commands.Handle("aimod", "policy/explain", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handlePolicyExplain)
	p.commands.Handle("aimod", "policy/set", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handlePolicySet)

	p.commands.Handle("aimod", "models/show", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModelsShow)
	p.commands.Handle("aimod", "models/compare", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModelsCompare)
	p.commands.Handle("aimod", "models/set-fast", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetFast)
	p.commands.Handle("aimod", "models/set-deep", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetDeep)

	// Autocomplete on every option whose values come from bot state, per
	// spec.MD section 4a. models/show is the no-typing discovery path that
	// the same rule requires alongside it.
	p.commands.Autocomplete("aimod", "models/set-fast", p.autocompleteModel)
	p.commands.Autocomplete("aimod", "models/set-deep", p.autocompleteModel)
	p.commands.Autocomplete("aimod", "models/compare", p.autocompleteModel)

	p.commands.Handle("aimod", "configure/key", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetKey)
	p.commands.Handle("aimod", "configure/mode", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetMode)
	p.commands.Handle("aimod", "configure/budget", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetBudget)
	p.commands.Handle("aimod", "configure/evidence", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetEvidence)
	p.commands.Handle("aimod", "configure/exempt-channel", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleExemptChannel)
	p.commands.Handle("aimod", "configure/exempt-role", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleExemptRole)
	p.commands.Handle("aimod", "configure/sanctions", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handleSetSanction)
	// actionPolicy rather than actionConfigure, matching sanctions: this
	// changes how much of the server actually gets looked at, which is a
	// policy decision rather than a plumbing one, and a guild can lower the
	// tier on it separately with /config permissions set-tier.
	p.commands.Handle("aimod", "configure/triage", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handleSetTriage)
	p.commands.Handle("aimod", "configure/show", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleConfigureShow)

	// Reading the calibration is a moderator's business: it explains why a
	// message was or was not touched. Changing it is not, because it changes
	// what the filter does, which is the same line actionPolicy already draws
	// around /aimod policy set.
	p.commands.Handle("aimod", "calibrate/show", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleCalibrateShow)
	p.commands.Handle("aimod", "calibrate/run-now", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handleCalibrateRunNow)
	p.commands.Handle("aimod", "calibrate/apply", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handleCalibrateApply)
	p.commands.Handle("aimod", "calibrate/clear", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handleCalibrateClear)
	p.commands.Handle("aimod", "calibrate/mode", core.PermSpec{Tier: core.TierAdmin, Action: actionPolicy}, p.handleCalibrateMode)

	// TierPublic, like moderate-me and for a related reason: the members
	// being asked to chip in have to be able to see the address and whether
	// it is needed. It shows two balances and a runway, and nothing about
	// what is being moderated or whom.
	p.commands.Handle("aimod", "funding/show", core.PermSpec{Tier: core.TierPublic}, p.handleFundingShow)
	// TierAdmin is the coarse floor; the real gate is canSetFunding inside
	// the handler, because no tier expresses "the guild owner". This is where
	// donated money goes, and a guild with five admins would otherwise have
	// five accounts that can silently repoint it.
	p.commands.Handle("aimod", "funding/set-address", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleFundingSetAddress)
	// No owner check on clear, deliberately: it can only ever stop donations,
	// never redirect them, so it is a kill switch that fails safe.
	p.commands.Handle("aimod", "funding/clear", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleFundingClear)
}
