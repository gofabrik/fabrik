// Package paging turns an untrusted page number and a total count into the
// numbers a store query and a set of page controls both need.
//
// It holds no data, runs no queries, and knows nothing about transport: the
// caller counts the matching rows and reads the requested page from wherever
// it came from. This package is arithmetic, and imports nothing.
package paging

// Page is one page of a result set. Build one with [Of]; the zero value
// describes nothing.
type Page struct {
	// Number is the 1-based page being shown, clamped to Pages, and 1
	// when nothing matched.
	Number int
	// Size is how many items a page holds, at least 1.
	Size int
	// Total is how many items matched across all pages.
	Total int
	// Pages is how many pages those items make, 0 when nothing matched.
	Pages int
}

// Of clamps requested against the pages that total and size actually make.
//
// A page number is navigation, not input to validate: page 0, a negative
// page, or a page past the end lands on a real page rather than an error, so
// a stale bookmark still shows results. That also covers unparseable input,
// since a failed strconv.Atoi leaves 0, which clamps to the first page.
//
// Because the number is clamped before [Page.Offset] multiplies it, an absurd
// request cannot overflow the offset.
//
// A size below 1 is treated as 1, and a negative total as 0, so neither can
// divide by zero or invent pages.
func Of(requested, size, total int) Page {
	size = max(size, 1)
	total = max(total, 0)

	// Not (total + size - 1) / size: that overflows on an enormous total.
	pages := total / size
	if total%size != 0 {
		pages++
	}

	number := requested
	switch {
	case pages == 0 || number < 1:
		number = 1
	case number > pages:
		number = pages
	}

	return Page{Number: number, Size: size, Total: total, Pages: pages}
}

// Limit is how many rows to ask the store for.
func (p Page) Limit() int { return p.Size }

// Offset is how many rows to skip.
func (p Page) Offset() int { return (p.Number - 1) * p.Size }

// Empty reports whether nothing matched at all.
func (p Page) Empty() bool { return p.Total == 0 }

// HasPrev reports whether there is a page before this one.
func (p Page) HasPrev() bool { return p.Number > 1 }

// HasNext reports whether there is a page after this one.
func (p Page) HasNext() bool { return p.Number < p.Pages }

// Prev is the page before this one, or this one when there is none, so a
// link built without checking [Page.HasPrev] still points somewhere real.
func (p Page) Prev() int {
	if p.HasPrev() {
		return p.Number - 1
	}
	return p.Number
}

// Next is the page after this one, or this one when there is none.
func (p Page) Next() int {
	if p.HasNext() {
		return p.Number + 1
	}
	return p.Number
}

// First is the 1-based position of this page's first item, and 0 when
// nothing matched.
func (p Page) First() int {
	if p.Empty() {
		return 0
	}
	return p.Offset() + 1
}

// Last is the 1-based position of this page's last item, and 0 when nothing
// matched. The final page is usually short, so this is not always
// First + Size - 1.
func (p Page) Last() int {
	if p.Empty() {
		return 0
	}
	// Offset is below Total here, so subtracting cannot overflow where
	// Offset+Size near MaxInt can.
	if p.Total-p.Offset() <= p.Size {
		return p.Total
	}
	return p.Offset() + p.Size
}
