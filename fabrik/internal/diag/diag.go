// Package diag defines fabrik's diagnostic model and a rustc-style terminal
// formatter for reporting directive errors and warnings.
package diag

import (
	"fmt"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Severity classifies a diagnostic. Errors are fatal to code generation;
// warnings are informational.
type Severity int

const (
	SevError Severity = iota
	SevWarning
)

// Diagnostic is a single problem found while scanning or generating, anchored
// to a source position with an optional actionable help line.
type Diagnostic struct {
	Severity Severity
	Pos      token.Position
	Message  string
	Help     string
}

// Diagnostics is an ordered collection of diagnostics.
type Diagnostics []Diagnostic

// HasFatal reports whether any diagnostic is an error.
func (ds Diagnostics) HasFatal() bool {
	for _, d := range ds {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

// Counts returns the number of errors and warnings.
func (ds Diagnostics) Counts() (errs, warns int) {
	for _, d := range ds {
		if d.Severity == SevError {
			errs++
		} else {
			warns++
		}
	}
	return
}

// Sort orders diagnostics by file, line, then column for stable output.
func (ds Diagnostics) Sort() {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i].Pos, ds[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
}

// --- formatter ---------------------------------------------------------------

const (
	ansiReset      = "\033[0m"
	ansiBold       = "\033[1m"
	ansiBoldRed    = "\033[1;31m"
	ansiBoldYellow = "\033[1;33m"
	ansiBoldBlue   = "\033[1;34m"
)

// Formatter renders diagnostics to a writer, rustc-style: a colored severity
// label, a source locator, the offending line with a caret span, and an
// optional help note. Color is enabled only when the writer is a terminal.
type Formatter struct {
	out   io.Writer
	color bool
	src   map[string][]string
	cwd   string
}

// NewFormatter returns a Formatter writing to out.
func NewFormatter(out io.Writer) *Formatter {
	f := &Formatter{
		out:   out,
		src:   make(map[string][]string),
		color: isTerminal(out),
	}
	if cwd, err := os.Getwd(); err == nil {
		f.cwd = cwd
	}
	return f
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := file.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (f *Formatter) paint(code, s string) string {
	if !f.color {
		return s
	}
	return code + s + ansiReset
}

func (f *Formatter) srcLine(filename string, num int) string {
	lines, ok := f.src[filename]
	if !ok {
		data, err := os.ReadFile(filename)
		if err != nil {
			return ""
		}
		lines = strings.Split(string(data), "\n")
		f.src[filename] = lines
	}
	if num < 1 || num > len(lines) {
		return ""
	}
	return lines[num-1]
}

func (f *Formatter) relPath(p string) string {
	if f.cwd == "" {
		return p
	}
	rel, err := filepath.Rel(f.cwd, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

// Emit writes a single diagnostic.
func (f *Formatter) Emit(d Diagnostic) {
	label, labelColor := "error", ansiBoldRed
	if d.Severity == SevWarning {
		label, labelColor = "warning", ansiBoldYellow
	}
	fmt.Fprintf(f.out, "%s: %s\n",
		f.paint(labelColor, label),
		f.paint(ansiBold, d.Message))

	fmt.Fprintf(f.out, "  %s %s:%d:%d\n",
		f.paint(ansiBoldBlue, "-->"),
		f.relPath(d.Pos.Filename), d.Pos.Line, d.Pos.Column)

	src := strings.TrimRight(f.srcLine(d.Pos.Filename, d.Pos.Line), " \t\r")
	if src != "" {
		lineNum := fmt.Sprintf("%d", d.Pos.Line)
		pad := strings.Repeat(" ", len(lineNum))
		bar := f.paint(ansiBoldBlue, "|")

		fmt.Fprintf(f.out, "  %s %s\n", pad, bar)
		fmt.Fprintf(f.out, "  %s %s %s\n", f.paint(ansiBoldBlue, lineNum), bar, src)

		col := d.Pos.Column - 1
		if col < 0 {
			col = 0
		}
		caretLen := caretSpanLen(src, col)
		indent := strings.Repeat(" ", col)
		carets := strings.Repeat("^", caretLen)
		fmt.Fprintf(f.out, "  %s %s %s%s\n", pad, bar, indent, f.paint(labelColor, carets))

		if d.Help != "" {
			fmt.Fprintf(f.out, "  %s %s\n", pad, bar)
			fmt.Fprintf(f.out, "  %s %s %s: %s\n",
				pad,
				f.paint(ansiBoldBlue, "="),
				f.paint(ansiBold, "help"),
				d.Help)
		}
	} else if d.Help != "" {
		fmt.Fprintf(f.out, "  %s %s: %s\n",
			f.paint(ansiBoldBlue, "="),
			f.paint(ansiBold, "help"),
			d.Help)
	}

	fmt.Fprintln(f.out)
}

// Summary writes a trailing one-line summary of the emitted counts.
func (f *Formatter) Summary(errs, warns int) {
	switch {
	case errs > 0:
		parts := []string{count(errs, "error", "errors")}
		if warns > 0 {
			parts = append(parts, count(warns, "warning", "warnings"))
		}
		fmt.Fprintf(f.out, "%s: aborting due to %s\n",
			f.paint(ansiBoldRed, "error"), joinWithAnd(parts))
	case warns > 0:
		fmt.Fprintf(f.out, "%s: %s emitted\n",
			f.paint(ansiBoldYellow, "warning"), count(warns, "warning", "warnings"))
	}
}

func count(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func joinWithAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// caretSpanLen returns the length of the token starting at col so the carets
// underline just that token (e.g. the //fabrik:NAME directive) and not its args.
func caretSpanLen(src string, col int) int {
	if col >= len(src) {
		return 1
	}
	end := col
	for end < len(src) && !unicode.IsSpace(rune(src[end])) {
		end++
	}
	n := end - col
	if n < 1 {
		n = 1
	}
	return n
}
