package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

func writeHelp(w io.Writer, args ...any) {
	//nolint:errcheck // Help rendering is best-effort and its void API has no error channel.
	fmt.Fprint(w, args...)
}

func writeHelpLine(w io.Writer, args ...any) {
	//nolint:errcheck // Help rendering is best-effort and its void API has no error channel.
	fmt.Fprintln(w, args...)
}

func writeHelpf(w io.Writer, format string, args ...any) {
	//nolint:errcheck // Help rendering is best-effort and its void API has no error channel.
	fmt.Fprintf(w, format, args...)
}

// renderHelp renders the final command in path with every inherited flag in known.
func renderHelp(w io.Writer, path []*Command, known []AnyFlag) {
	info := detectTermInfo(w)
	cmd := path[len(path)-1]
	names := strings.Join(commandNames(path), " ")

	usage := cmd.Usage
	if usage == "" {
		usage = names
		if hasVisibleFlags(known) {
			usage += " [flags]"
		}
		if hasVisibleSubcommands(cmd.Subcommands) {
			usage += " <command>"
		}
		for _, a := range cmd.Args {
			usage += " " + renderArgSlot(a)
		}
	}
	writeHelpLine(w, info.bold("Usage:"))
	writeHelpLine(w, "  "+usage)
	writeHelpLine(w)

	if cmd.Long != "" {
		writeHelpLine(w, cmd.Long)
		writeHelpLine(w)
	} else if cmd.Help != "" {
		writeHelpLine(w, cmd.Help)
		writeHelpLine(w)
	}

	if len(cmd.Args) > 0 {
		writeHelpLine(w, info.bold("Arguments:"))
		renderArgs(w, info, cmd.Args)
		writeHelpLine(w)
	}

	if hasVisibleFlags(known) {
		renderFlags(w, info, known)
	}

	if hasVisibleSubcommands(cmd.Subcommands) {
		writeHelpLine(w, info.bold("Commands:"))
		renderSubcommands(w, cmd.Subcommands)
		writeHelpLine(w)
		writeHelpf(w, "Use \"%s <command> --help\" for more information about a command.\n", names)
		writeHelpLine(w)
	}

	if len(cmd.Examples) > 0 {
		writeHelpLine(w, info.bold("Examples:"))
		for _, ex := range cmd.Examples {
			if ex.Help != "" {
				writeHelpf(w, "  # %s\n", ex.Help)
			}
			writeHelpf(w, "  %s\n\n", ex.Cmd)
		}
	}
}

func renderArgSlot(a AnyArg) string {
	name := a.argName()
	switch {
	case a.argVariadic() && a.argRequired():
		return fmt.Sprintf("<%s...>", name)
	case a.argVariadic():
		return fmt.Sprintf("[%s...]", name)
	case a.argRequired():
		return fmt.Sprintf("<%s>", name)
	default:
		return fmt.Sprintf("[%s]", name)
	}
}

func renderArgs(w io.Writer, info termInfo, args []AnyArg) {
	width := 0
	for _, a := range args {
		if l := len(a.argName()); l > width {
			width = l
		}
	}
	for _, a := range args {
		prefix := fmt.Sprintf("  %-*s  ", width, a.argName())
		printWrapped(w, prefix, info.width, a.argHelp())
	}
}

func renderFlags(w io.Writer, info termInfo, flags []AnyFlag) {
	visible := make([]AnyFlag, 0, len(flags))
	for _, f := range flags {
		if !f.flagHidden() {
			visible = append(visible, f)
		}
	}
	if len(visible) == 0 {
		return
	}

	// Ungrouped flags render before named groups.
	groups := map[string][]AnyFlag{}
	order := []string{""}
	for _, f := range visible {
		g := f.flagGroup()
		if _, seen := groups[g]; !seen && g != "" {
			order = append(order, g)
		}
		groups[g] = append(groups[g], f)
	}

	for _, g := range order {
		fs := groups[g]
		if len(fs) == 0 {
			continue
		}
		if g == "" {
			writeHelpLine(w, info.bold("Flags:"))
		} else {
			writeHelpLine(w, info.bold(g+":"))
		}
		sort.SliceStable(fs, func(i, j int) bool {
			return fs[i].flagName() < fs[j].flagName()
		})

		left := make([]string, len(fs))
		colWidth := 0
		for i, f := range fs {
			left[i] = renderFlagLeft(f)
			if l := len(left[i]); l > colWidth {
				colWidth = l
			}
		}
		for i, f := range fs {
			right := renderFlagRight(f)
			prefix := fmt.Sprintf("  %-*s  ", colWidth, left[i])
			printWrapped(w, prefix, info.width, right)
		}
		writeHelpLine(w)
	}
}

func renderFlagLeft(f AnyFlag) string {
	parts := []string{}
	if f.flagShort() != 0 {
		parts = append(parts, "-"+string(f.flagShort())+",")
	} else {
		parts = append(parts, "   ")
	}
	long := "--" + f.flagName()
	if !f.flagIsBool() {
		long += " " + f.flagPlaceholder()
	}
	parts = append(parts, long)
	return strings.Join(parts, " ")
}

func renderFlagRight(f AnyFlag) string {
	var sb strings.Builder
	sb.WriteString(f.flagHelp())
	annot := []string{}
	if f.flagRequired() {
		annot = append(annot, "required")
	}
	if f.flagHasDefault() {
		annot = append(annot, "default: "+f.flagDefaultText())
	}
	if f.flagEnv() != "" {
		annot = append(annot, "env: "+f.flagEnv())
	}
	if len(annot) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("  ")
		}
		sb.WriteString("(" + strings.Join(annot, ", ") + ")")
	}
	return sb.String()
}

func renderSubcommands(w io.Writer, subs []*Command) {
	visible := make([]*Command, 0, len(subs))
	for _, s := range subs {
		if !s.Hidden {
			visible = append(visible, s)
		}
	}
	width := 0
	for _, s := range visible {
		if l := len(listingName(s)); l > width {
			width = l
		}
	}
	sort.SliceStable(visible, func(i, j int) bool {
		return visible[i].Name < visible[j].Name
	})
	for _, s := range visible {
		writeHelpf(w, "  %-*s  %s\n", width, listingName(s), s.Help)
	}
}

func listingName(c *Command) string {
	if len(c.Aliases) == 0 {
		return c.Name
	}
	return c.Name + ", " + strings.Join(c.Aliases, ", ")
}

func hasVisibleFlags(flags []AnyFlag) bool {
	for _, f := range flags {
		if !f.flagHidden() {
			return true
		}
	}
	return false
}

func hasVisibleSubcommands(subs []*Command) bool {
	for _, s := range subs {
		if !s.Hidden {
			return true
		}
	}
	return false
}

// printWrapped aligns continuation lines with text and keeps at least 20 columns available.
func printWrapped(w io.Writer, prefix string, width int, text string) {
	avail := width - len(prefix)
	if avail < 20 {
		avail = 20
	}
	if text == "" {
		writeHelpLine(w, prefix)
		return
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		writeHelpLine(w, prefix)
		return
	}
	indent := strings.Repeat(" ", len(prefix))
	writeHelp(w, prefix)
	line := ""
	for _, word := range words {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) > avail:
			writeHelpLine(w, line)
			writeHelp(w, indent)
			line = word
		default:
			line += " " + word
		}
	}
	writeHelpLine(w, line)
}
