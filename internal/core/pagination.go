package core

import (
	"fmt"
	"strconv"
	"strings"
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
