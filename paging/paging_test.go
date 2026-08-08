package paging_test

import (
	"math"
	"testing"

	"github.com/gofabrik/fabrik/paging"
)

func TestOf(t *testing.T) {
	tests := []struct {
		name                    string
		requested, size, total  int
		number, pages, offset   int
		hasPrev, hasNext, empty bool
		first, last             int
	}{
		{
			name: "first of several", requested: 1, size: 10, total: 95,
			number: 1, pages: 10, offset: 0, hasNext: true, first: 1, last: 10,
		},
		{
			name: "middle", requested: 5, size: 10, total: 95,
			number: 5, pages: 10, offset: 40, hasPrev: true, hasNext: true, first: 41, last: 50,
		},
		{
			name: "short final page", requested: 10, size: 10, total: 95,
			number: 10, pages: 10, offset: 90, hasPrev: true, first: 91, last: 95,
		},
		{
			name: "exact multiple has no trailing page", requested: 1, size: 10, total: 100,
			number: 1, pages: 10, offset: 0, hasNext: true, first: 1, last: 10,
		},
		{
			name: "single item", requested: 1, size: 10, total: 1,
			number: 1, pages: 1, offset: 0, first: 1, last: 1,
		},
		{
			name: "nothing matched", requested: 1, size: 10, total: 0,
			number: 1, pages: 0, offset: 0, empty: true, first: 0, last: 0,
		},
		{
			name: "page past the end is clamped", requested: 999, size: 10, total: 95,
			number: 10, pages: 10, offset: 90, hasPrev: true, first: 91, last: 95,
		},
		{
			name: "page past the end of nothing", requested: 999, size: 10, total: 0,
			number: 1, pages: 0, offset: 0, empty: true, first: 0, last: 0,
		},
		{
			name: "zero page", requested: 0, size: 10, total: 95,
			number: 1, pages: 10, offset: 0, hasNext: true, first: 1, last: 10,
		},
		{
			name: "negative page", requested: -5, size: 10, total: 95,
			number: 1, pages: 10, offset: 0, hasNext: true, first: 1, last: 10,
		},
		{
			name: "absurd page cannot overflow the offset", requested: math.MaxInt, size: 10, total: 95,
			number: 10, pages: 10, offset: 90, hasPrev: true, first: 91, last: 95,
		},
		{
			name: "size below one is treated as one", requested: 3, size: 0, total: 5,
			number: 3, pages: 5, offset: 2, hasPrev: true, hasNext: true, first: 3, last: 3,
		},
		{
			name: "negative total invents no pages", requested: 2, size: 10, total: -7,
			number: 1, pages: 0, offset: 0, empty: true, first: 0, last: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := paging.Of(tt.requested, tt.size, tt.total)

			if p.Number != tt.number {
				t.Errorf("Number = %d, want %d", p.Number, tt.number)
			}
			if p.Pages != tt.pages {
				t.Errorf("Pages = %d, want %d", p.Pages, tt.pages)
			}
			if p.Offset() != tt.offset {
				t.Errorf("Offset = %d, want %d", p.Offset(), tt.offset)
			}
			if p.HasPrev() != tt.hasPrev {
				t.Errorf("HasPrev = %v, want %v", p.HasPrev(), tt.hasPrev)
			}
			if p.HasNext() != tt.hasNext {
				t.Errorf("HasNext = %v, want %v", p.HasNext(), tt.hasNext)
			}
			if p.Empty() != tt.empty {
				t.Errorf("Empty = %v, want %v", p.Empty(), tt.empty)
			}
			if p.First() != tt.first {
				t.Errorf("First = %d, want %d", p.First(), tt.first)
			}
			if p.Last() != tt.last {
				t.Errorf("Last = %d, want %d", p.Last(), tt.last)
			}
			if p.Limit() != max(tt.size, 1) {
				t.Errorf("Limit = %d, want %d", p.Limit(), max(tt.size, 1))
			}
		})
	}
}

// Walking every page must visit each item exactly once, with no gap at a page
// boundary and no overrun at the end.
func TestOfCoversEveryItemOnce(t *testing.T) {
	for _, total := range []int{0, 1, 9, 10, 11, 99, 100} {
		for _, size := range []int{1, 3, 10} {
			seen := 0
			p := paging.Of(1, size, total)
			for page := 1; page <= p.Pages; page++ {
				at := paging.Of(page, size, total)
				if at.Offset() != seen {
					t.Fatalf("total=%d size=%d page=%d: offset %d, want %d",
						total, size, page, at.Offset(), seen)
				}
				seen += min(at.Limit(), total-at.Offset())
			}
			if seen != total {
				t.Errorf("total=%d size=%d: covered %d items", total, size, seen)
			}
		}
	}
}

func TestPrevNextStayOnRealPages(t *testing.T) {
	only := paging.Of(1, 10, 5)
	if only.Prev() != 1 || only.Next() != 1 {
		t.Errorf("single page: prev=%d next=%d, want 1 and 1", only.Prev(), only.Next())
	}

	none := paging.Of(1, 10, 0)
	if none.Prev() != 1 || none.Next() != 1 {
		t.Errorf("no pages: prev=%d next=%d, want 1 and 1", none.Prev(), none.Next())
	}

	middle := paging.Of(2, 10, 95)
	if middle.Prev() != 1 || middle.Next() != 3 {
		t.Errorf("middle: prev=%d next=%d, want 1 and 3", middle.Prev(), middle.Next())
	}
}

func TestLastNearMaxInt(t *testing.T) {
	p := paging.Of(math.MaxInt, 10, math.MaxInt)
	if got := p.Last(); got != math.MaxInt {
		t.Fatalf("Last() = %d, want %d", got, math.MaxInt)
	}
	if p.First() > p.Last() {
		t.Fatalf("First() = %d beyond Last() = %d", p.First(), p.Last())
	}
}
