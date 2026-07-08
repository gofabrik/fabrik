// Package diagfmt renders diagnostics for the terminal.
package diagfmt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gofabrik/fabrik/diag"
)

const (
	ansiReset      = "\033[0m"
	ansiBold       = "\033[1m"
	ansiBoldRed    = "\033[1;31m"
	ansiBoldYellow = "\033[1;33m"
	ansiBoldBlue   = "\033[1;34m"
)

// Formatter renders diagnostics to a writer.
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
func (f *Formatter) Emit(d diag.Diagnostic) {
	label, labelColor := "error", ansiBoldRed
	if d.Severity == diag.SevWarning {
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

// caretSpanLen returns the length of the token starting at col.
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
