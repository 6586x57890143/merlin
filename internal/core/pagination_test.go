package core

import (
	"testing"
)

func TestPaginateSinglePage(t *testing.T) {
	items := []int{1, 2, 3}
	page, clamped, total := Paginate(items, 0)
	if total != 1 || clamped != 0 {
		t.Fatalf("expected 1 total page, clamped 0, got total=%d clamped=%d", total, clamped)
	}
	if len(page) != 3 {
		t.Fatalf("expected all 3 items on the only page, got %d", len(page))
	}
}

func TestPaginateMultiplePages(t *testing.T) {
	items := make([]int, PageSize*2+3) // 2 full pages + 1 partial
	for i := range items {
		items[i] = i
	}

	page0, clamped0, total := Paginate(items, 0)
	if total != 3 {
		t.Fatalf("expected 3 total pages, got %d", total)
	}
	if clamped0 != 0 || len(page0) != PageSize {
		t.Fatalf("expected a full first page, got clamped=%d len=%d", clamped0, len(page0))
	}

	lastPage, clampedLast, _ := Paginate(items, 2)
	if clampedLast != 2 || len(lastPage) != 3 {
		t.Fatalf("expected the last page to have the remaining 3 items, got clamped=%d len=%d", clampedLast, len(lastPage))
	}
}

func TestPaginateClampsOutOfRangePage(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}

	if _, clamped, _ := Paginate(items, 999); clamped != 0 {
		t.Fatalf("expected an absurdly high page to clamp to the only valid page (0), got %d", clamped)
	}
	if _, clamped, _ := Paginate(items, -5); clamped != 0 {
		t.Fatalf("expected a negative page to clamp to 0, got %d", clamped)
	}
}

func TestPaginateEmptyList(t *testing.T) {
	page, clamped, total := Paginate([]int{}, 0)
	if total != 1 || clamped != 0 || page != nil {
		t.Fatalf("expected an empty list to report 1 (empty) page, got total=%d clamped=%d page=%+v", total, clamped, page)
	}
}

func TestPaginationCustomIDRoundTrip(t *testing.T) {
	id := PaginationCustomID("rotation:list:", 4)
	page, err := ParsePaginationPage(id, "rotation:list:")
	if err != nil {
		t.Fatalf("ParsePaginationPage: %v", err)
	}
	if page != 4 {
		t.Fatalf("expected page 4 round-tripped, got %d", page)
	}
}

func TestParsePaginationPageRejectsGarbage(t *testing.T) {
	if _, err := ParsePaginationPage("rotation:list:not-a-number", "rotation:list:"); err == nil {
		t.Fatal("expected an error parsing a non-numeric page suffix")
	}
}
