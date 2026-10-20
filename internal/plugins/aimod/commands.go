package aimod

import (
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/discord"
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
	actionWhy       = "aimod.why"
	actionUndo      = "aimod.undo"
	actionPolicy    = "aimod.policy"
	actionConfigure = "aimod.configure"
)

func (p *Plugin) registerCommands() {

	messageOpt := discord.ApplicationCommandOptionString{
		Name:        "message_id",
		Description: "The message's ID (Developer Mode: right-click the message, Copy Message ID)", Required: true,
	}

	cmd := discord.SlashCommandCreate{
		Name:        "aimod",
		Description: "AI moderation against Discord's Community Guidelines",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "settings",
				Description: "Mode, key, budget, exemptions, policy, models, word list and calibration, on one panel",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "status",
				Description: "What this server is scanning, what it has spent today, and whether anything is wrong",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "why",
				Description: "Show why a message was rewritten, using the code under it",
				Options: []discord.ApplicationCommandOption{discord.ApplicationCommandOptionString{
					Name:        "id",
					Description: "The code under a rewritten message (mods may also give a message ID)", Required: true,
				}},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "undo",
				Description: "Reverse a false positive: repost the original message and mark the incident undone",
				Options:     []discord.ApplicationCommandOption{messageOpt},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "moderate-me",
				Description: "Ask to be jailed by the automatic sanctions like anyone else, even if you are staff",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionBool{
						Name:        "enabled",
						Description: "True to opt in, false to go back to the default", Required: true,
					},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "opt-out",
				Description: "Ask not to have your messages read by the AI filter (if this server allows it)",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionBool{
						Name:        "enabled",
						Description: "True to opt out, false to be covered again", Required: true,
					},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "moderate-user",
				Description: "Operator only: put somebody else on the automatic-sanction list",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionUser{
						Name:        "user",
						Description: "The member", Required: true,
					},
					discord.ApplicationCommandOptionBool{
						Name:        "enabled",
						Description: "True to opt them in, false to remove them", Required: true,
					},
				},
			},
			discord.ApplicationCommandOptionSubCommandGroup{
				Name:        "models",
				Description: "Which models each pass uses, what they cost, and what that projects to per day",
				Options: []discord.ApplicationCommandOptionSubCommand{
					{
						Name:        "show",
						Description: "Current models with live prices, this server's spend, and a projected daily cost",
						Options: []discord.ApplicationCommandOption{
							discord.ApplicationCommandOptionInt{
								Name:        "messages_per_day",
								Description: "Project against this volume instead of this server's measured traffic",
							},
						},
					},
					{
						Name:        "set-fast",
						Description: "The cheap pass that reads every message. Comma separated; later entries are fallbacks.",
						Options: []discord.ApplicationCommandOption{
							discord.ApplicationCommandOptionString{
								Name:        "models",
								Description: "Model IDs, cheapest first. Type to search; \"default\" tracks merlin's own defaults.",
								Required:    true, Autocomplete: true,
							},
						},
					},
					{
						Name:        "set-deep",
						Description: "The pass that confirms a flag before anything is deleted. Worth spending on.",
						Options: []discord.ApplicationCommandOption{
							discord.ApplicationCommandOptionString{
								Name:        "models",
								Description: "Model IDs, best first. Type to search; \"default\" tracks merlin's own defaults.",
								Required:    true, Autocomplete: true,
							},
						},
					},
					{
						Name:        "compare",
						Description: "Price a model against this server's measured traffic before switching to it",
						Options: []discord.ApplicationCommandOption{
							discord.ApplicationCommandOptionString{
								Name:        "model",
								Description: "The model to price", Required: true, Autocomplete: true,
							},
							discord.ApplicationCommandOptionString{
								Name:        "pass",
								Description: "Which pass it would run (default: fast)",
								Choices: []discord.ApplicationCommandOptionChoiceString{
									{Name: "fast", Value: "fast"},
									{Name: "deep", Value: "deep"},
								},
							},
						},
					},
				},
			},
			discord.ApplicationCommandOptionSubCommandGroup{
				Name:        "funding",
				Description: "The tip jar that pays for the scanning, and how much credit is left",
				Options: []discord.ApplicationCommandOptionSubCommand{
					{
						Name:        "show",
						Description: "How much scanning credit is left, and where to chip in",
					},
					{
						Name:        "set-address",
						Description: "Point the tip jar at a wallet (server owner only)",
						Options: []discord.ApplicationCommandOption{
							discord.ApplicationCommandOptionString{
								Name:        "address",
								Description: "An EVM (0x...), TRON (T...) or Solana wallet address",
								Required:    true,
							},
						},
					},
					{
						Name:        "clear",
						Description: "Remove the tip jar and stop reading its balance",
					},
				},
			},
			discord.ApplicationCommandOptionSubCommandGroup{
				Name:        "calibrate",
				Description: "The weekly review that tunes the filter to how this server actually talks",
				Options: []discord.ApplicationCommandOptionSubCommand{
					{
						Name:        "show",
						Description: "What the filter has learned, and anything waiting to be applied",
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)
	p.commands.HandleIx("aimod", "settings", viewSpec, p.handleSettings)
	p.registerPanel()

	p.commands.HandleIx("aimod", "status", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleStatus)
	// Same spec as the command it pages: a component interaction is its own
	// fresh interaction and gets no exemption from authorization just because
	// the message carrying the button was rendered for somebody allowed to
	// see it. Same rule adminconfig's wizard follows.
	p.commands.HandleComponentIx(p.Name(), statusPagePrefix, core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleStatusPage)
	// TierPublic: the code is printed under the repost for everybody in the
	// channel, so answering it is the point. What a member is shown is cut
	// down inside the handler; the moderator's view still needs actionRead.
	p.commands.HandleIx("aimod", "why", core.PermSpec{Tier: core.TierPublic, Action: actionWhy}, p.handleWhy)
	p.commands.HandleIx("aimod", "undo", core.PermSpec{Tier: core.TierMod, Action: actionUndo}, p.handleUndo)

	// TierPublic: consenting to be moderated is not a privilege, and gating
	// it behind a tier would mean the members most likely to want it (staff,
	// who are the only ones it changes anything for) are also the only ones
	// who could reach it, which is backwards.
	p.commands.HandleIx("aimod", "moderate-me", core.PermSpec{Tier: core.TierPublic}, p.handleModerateMe)
	// TierMod is the coarse gate; the real one is the operator check inside
	// the handler, because no tier expresses "one named identity".
	p.commands.HandleIx("aimod", "moderate-user", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModerateUser)

	// TierPublic for the same reason as moderate-me, and the mirror image of
	// it: withholding consent is not a privilege either. The gate that
	// matters is the guild's own switch, checked inside the handler, and the
	// rule that a member sets only themselves. See optout.go.
	p.commands.HandleIx("aimod", "opt-out", core.PermSpec{Tier: core.TierPublic}, p.handleOptOut)

	p.commands.HandleIx("aimod", "models/show", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModelsShow)
	p.commands.HandleIx("aimod", "models/compare", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleModelsCompare)
	// The panel's models form is a modal, and a modal cannot autocomplete, so
	// these are the search-as-you-type path to the same setStack.
	p.commands.HandleIx("aimod", "models/set-fast", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetStack(true))
	p.commands.HandleIx("aimod", "models/set-deep", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleSetStack(false))

	// Autocomplete on every option whose values come from bot state, per
	// spec.MD section 4a. models/show is the no-typing discovery path that
	// the same rule requires alongside it.
	p.commands.AutocompleteIx("aimod", "models/compare", p.autocompleteModel)
	p.commands.AutocompleteIx("aimod", "models/set-fast", p.autocompleteModel)
	p.commands.AutocompleteIx("aimod", "models/set-deep", p.autocompleteModel)

	// actionPolicy rather than actionConfigure, matching sanctions: this
	// changes how much of the server actually gets looked at, which is a
	// policy decision rather than a plumbing one, and a guild can lower the
	// tier on it separately with /config permissions set-tier.
	// actionPolicy rather than actionConfigure, matching triage and sanctions:
	// this decides how much of the server gets looked at. TierAdmin is only
	// the coarse floor; the real gate is ownerOrOperator inside the handler,
	// because no tier expresses "the guild owner", and this is the one
	// setting here that makes the filter cover *less*.

	// actionPolicy, like policy set: the list decides what gets rewritten or
	// removed in this server.

	// Reading the calibration is a moderator's business: it explains why a
	// message was or was not touched. Changing it is not, because it changes
	// what the filter does, which is the same line actionPolicy already draws
	// around /aimod policy set.
	p.commands.HandleIx("aimod", "calibrate/show", core.PermSpec{Tier: core.TierMod, Action: actionRead}, p.handleCalibrateShow)

	// TierPublic, like moderate-me and for a related reason: the members
	// being asked to chip in have to be able to see the address and whether
	// it is needed. It shows two balances and a runway, and nothing about
	// what is being moderated or whom.
	p.commands.HandleIx("aimod", "funding/show", core.PermSpec{Tier: core.TierPublic}, p.handleFundingShow)
	// TierAdmin is the coarse floor; the real gate is ownerOrOperator inside
	// the handler, because no tier expresses "the guild owner". This is where
	// donated money goes, and a guild with five admins would otherwise have
	// five accounts that can silently repoint it.
	p.commands.HandleIx("aimod", "funding/set-address", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleFundingSetAddress)
	// No owner check on clear, deliberately: it can only ever stop donations,
	// never redirect them, so it is a kill switch that fails safe.
	p.commands.HandleIx("aimod", "funding/clear", core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}, p.handleFundingClear)
}
