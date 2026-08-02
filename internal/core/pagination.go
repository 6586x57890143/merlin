package core

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// PageSize is the number of list entries shown per page across every
// paginated list in this bot, one shared constant so pagination feels
// consistent regardless of which plugin's list you're looking at.
const PageSize = 10

// Paginate slices items into pages of PageSize, clamping page into range so
// a stale or hand-edited page number arriving via a button's CustomID can
// never index out of bounds. Returns the slice for that page, the page
// index it actually used (after clamping), and the total page count
// (always at least 1, even for an empty list).
func Paginate[T any](items []T, page int) (pageItems []T, clampedPage, totalPages int) {
	totalPages = max(1, (len(items)+PageSize-1)/PageSize)
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * PageSize
	end := min(start+PageSize, len(items))
	if start >= len(items) {
		return nil, page, totalPages
	}
	return items[start:end], page, totalPages
}

// PaginationCustomID builds the CustomID for a pagination button: prefix
// (the same one passed to CommandRouter.HandleComponent) followed by the
// target page number, so ParsePaginationPage can recover it on the other
// side of the click with no server-side session to consult.
func PaginationCustomID(prefix string, page int) string {
	return prefix + strconv.Itoa(page)
}

// ParsePaginationPage extracts the page number from a CustomID built by
// PaginationCustomID, given the same prefix.
func ParsePaginationPage(customID, prefix string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(customID, prefix))
	if err != nil {
		return 0, fmt.Errorf("core: invalid pagination page in custom ID %q: %w", customID, err)
	}
	return n, nil
}

// PaginationRow builds the Prev/Next button row for page (0-based) out of
// totalPages, disabling whichever end is already at its limit, with a
// disabled middle button showing "page X/Y" for orientation. Returns nil if
// there's only one page: no controls needed at all, so a short list stays
// exactly as plain as it was before pagination existed.
func PaginationRow(prefix string, page, totalPages int) []discordgo.MessageComponent {
	if totalPages <= 1 {
		return nil
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "◀ Prev",
				Style:    discordgo.SecondaryButton,
				CustomID: PaginationCustomID(prefix, page-1),
				Disabled: page <= 0,
			},
			discordgo.Button{
				Label:    fmt.Sprintf("Page %d/%d", page+1, totalPages),
				Style:    discordgo.SecondaryButton,
				CustomID: prefix + "noop",
				Disabled: true,
			},
			discordgo.Button{
				Label:    "Next ▶",
				Style:    discordgo.SecondaryButton,
				CustomID: PaginationCustomID(prefix, page+1),
				Disabled: page >= totalPages-1,
			},
		}},
	}
}

// RespondEmbedWithComponents is RespondEmbed's counterpart for a response
// that also needs message components (pagination buttons, a select menu). It
// still attaches the brand avatar file the embed's footer icon references.
func RespondEmbedWithComponents(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
			Files:      []*discordgo.File{avatarFile()},
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	})
}

// UpdateEmbedWithComponents edits the message a component interaction (a
// Prev/Next click, a select-menu choice) arrived on in place, rather than
// sending a new one, the correct response type for any ComponentHandler
// that's re-rendering the same list/detail view the user is already
// looking at.
func UpdateEmbedWithComponents(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) error {
	return updateEmbed(s, i, embed, components, []*discordgo.File{avatarFile()})
}

// UpdateLandmarkEmbedWithComponents is UpdateEmbedWithComponents for a
// NewLandmarkEmbed. Also re-uploads the banner file its Image references.
func UpdateLandmarkEmbedWithComponents(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) error {
	return updateEmbed(s, i, embed, components, []*discordgo.File{avatarFile(), bannerFile()})
}

func updateEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) error {
	// Empty (not omitted) attachments means "keep none of what's already on
	// the message": the newly uploaded files are the whole set afterwards.
	// Omitting it instead retains the old ones and appends these, so a view
	// edited repeatedly (a paginated list, /config setup's wizard) would
	// accumulate a duplicate avatar per click until the embed's
	// attachment:// references stopped resolving to the right file.
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:      []*discordgo.MessageEmbed{embed},
			Components:  components,
			Files:       files,
			Attachments: &[]*discordgo.MessageAttachment{},
		},
	})
}
