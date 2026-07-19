package cli

import (
	"fmt"
	"reflect"
	"strings"
)

// ParseResult contains the resolved command, parsed values, and help or version state from [Command.Parse].
type ParseResult struct {
	cmd     *Command
	path    []*Command
	values  values
	help    bool
	version bool
}

// parseArgs returns partial state on errors so the renderer can show help for the deepest resolved command.
func parseArgs(root *Command, tokens []string, env func(string) (string, bool)) (*ParseResult, []AnyFlag, error) {
	if err := validateArgsDecl(root, nil); err != nil {
		return &ParseResult{cmd: root, path: []*Command{root}}, root.Flags, err
	}

	res := &ParseResult{
		cmd:    root,
		path:   []*Command{root},
		values: values{},
	}
	known := append([]AnyFlag(nil), root.Flags...)
	positional := []string(nil)
	// Positionals stop command descent, while only variadics also stop flag parsing.
	positionalsStarted := false
	positionalLocked := false

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		if tok == "--" {
			positional = append(positional, tokens[i+1:]...)
			break
		}

		if tok == "--help" || tok == "-h" {
			res.help = true
			return res, known, nil
		}

		// --version remains available as a user flag when Version is empty.
		if tok == "--version" && res.cmd.Version != "" {
			res.version = true
			return res, known, nil
		}

		if !positionalLocked && isFlagToken(tok) {
			next := ""
			haveNext := i+1 < len(tokens)
			if haveNext {
				next = tokens[i+1]
			}
			consumed, err := applyFlagToken(tok, next, haveNext, known, res.values)
			if err != nil {
				return res, known, err
			}
			i += consumed
			continue
		}

		if !positionalsStarted {
			if sub := findSub(res.cmd, tok); sub != nil {
				res.cmd = sub
				res.path = append(res.path, sub)
				known = append(known, sub.Flags...)
				continue
			}
			if res.cmd.Run == nil && len(res.cmd.Subcommands) > 0 {
				return res, known, unknownSubcommand(res.cmd, tok)
			}
		}
		positionalsStarted = true
		if commandHasVariadic(res.cmd) {
			positionalLocked = true
		}
		positional = append(positional, tok)
	}

	// Injected completion commands bypass inherited environment, required, and validation policy.
	if !res.cmd.injected {
		for _, f := range known {
			if err := f.flagApplyEnv(res.values, env); err != nil {
				return res, known, err
			}
		}
		for _, f := range known {
			if f.flagRequired() && !f.flagPresent(res.values) && !f.flagHasDefault() {
				return res, known, fmt.Errorf("required flag --%s not set: %w", f.flagName(), ErrUsage)
			}
			if err := f.flagValidate(res.values); err != nil {
				return res, known, err
			}
		}
	}

	// Grouping-only commands use a distinct sentinel so the renderer can show help without an error line.
	if res.cmd.Run == nil && len(res.cmd.Subcommands) > 0 {
		return res, known, ErrMissingSubcommand
	}

	if err := bindPositionals(res.cmd.Args, positional, res.values); err != nil {
		return res, known, err
	}
	for _, a := range res.cmd.Args {
		if err := a.argValidate(res.values); err != nil {
			return res, known, err
		}
	}

	return res, known, nil
}

// validateArgsDecl rejects nil entries, invalid argument order or variadic shape, and duplicate or unreachable names in a command subtree.
func validateArgsDecl(c *Command, inherited []AnyFlag) error {
	longSeen := map[string]bool{}
	shortSeen := map[rune]bool{}
	for _, f := range inherited {
		longSeen[f.flagName()] = true
		if r := f.flagShort(); r != 0 {
			shortSeen[r] = true
		}
	}
	for _, f := range c.Flags {
		if isNilValue(f) {
			return fmt.Errorf("command %q: has a nil flag", c.Name)
		}
		name := f.flagName()
		// Built-in help interception makes these names unreachable.
		if name == "help" {
			return fmt.Errorf("command %q: flag --help is reserved for built-in help", c.Name)
		}
		if f.flagShort() == 'h' {
			return fmt.Errorf("command %q: short flag -h is reserved for built-in help", c.Name)
		}
		if longSeen[name] {
			return fmt.Errorf("command %q: duplicate flag --%s", c.Name, name)
		}
		longSeen[name] = true
		if r := f.flagShort(); r != 0 {
			if shortSeen[r] {
				return fmt.Errorf("command %q: duplicate short flag -%c", c.Name, r)
			}
			shortSeen[r] = true
		}
	}
	// A declared Version makes an own or inherited version flag unreachable.
	if c.Version != "" && longSeen["version"] {
		return fmt.Errorf("command %q: flag --version is reserved (the command declares a Version)", c.Name)
	}

	seenOptional := false
	for i, a := range c.Args {
		if isNilValue(a) {
			return fmt.Errorf("command %q: has a nil arg", c.Name)
		}
		if a.argVariadic() && i != len(c.Args)-1 {
			return fmt.Errorf("command %q: variadic arg %q must be last", c.Name, a.argName())
		}
		if a.argRequired() && seenOptional {
			return fmt.Errorf("command %q: required arg %q must precede optional args", c.Name, a.argName())
		}
		if a.argIsSlice() && !a.argVariadic() {
			return fmt.Errorf("command %q: slice arg %q must be declared Variadic", c.Name, a.argName())
		}
		if a.argVariadic() && !a.argIsSlice() {
			return fmt.Errorf("command %q: variadic arg %q must be a slice type", c.Name, a.argName())
		}
		if !a.argRequired() && !a.argVariadic() {
			seenOptional = true
		}
	}

	nameSeen := map[string]bool{}
	for _, s := range c.Subcommands {
		if s == nil {
			return fmt.Errorf("command %q: has a nil subcommand", c.Name)
		}
		if nameSeen[s.Name] {
			return fmt.Errorf("command %q: duplicate subcommand %q", c.Name, s.Name)
		}
		nameSeen[s.Name] = true
	}
	childInherited := append(append([]AnyFlag(nil), inherited...), c.Flags...)
	for _, s := range c.Subcommands {
		if err := validateArgsDecl(s, childInherited); err != nil {
			return err
		}
	}
	return nil
}

// isNilValue detects typed nil pointers hidden inside flag or argument interfaces.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

func findSub(c *Command, name string) *Command {
	for _, s := range c.Subcommands {
		if s != nil && s.Name == name {
			return s
		}
	}
	return nil
}

// commandHasVariadic reports whether positionals must stop flag parsing.
func commandHasVariadic(c *Command) bool {
	for _, a := range c.Args {
		if a.argVariadic() {
			return true
		}
	}
	return false
}

func findFlagLong(flags []AnyFlag, name string) AnyFlag {
	for _, f := range flags {
		if f.flagName() == name {
			return f
		}
	}
	return nil
}

func findFlagShort(flags []AnyFlag, r rune) AnyFlag {
	for _, f := range flags {
		if f.flagShort() == r && r != 0 {
			return f
		}
	}
	return nil
}

func isFlagToken(tok string) bool {
	return len(tok) >= 2 && tok[0] == '-' && tok != "--"
}

// applyFlagToken returns whether it consumed the following token as a value.
func applyFlagToken(tok, next string, haveNext bool, flags []AnyFlag, vs values) (int, error) {
	isLong := strings.HasPrefix(tok, "--")
	body := tok[1:]
	if isLong {
		body = tok[2:]
	}

	name, valIn, hasInline := body, "", false
	if i := strings.Index(body, "="); i >= 0 {
		name = body[:i]
		valIn = body[i+1:]
		hasInline = true
	}

	var f AnyFlag
	if isLong {
		f = findFlagLong(flags, name)
		if f == nil {
			return 0, fmt.Errorf("unknown flag --%s: %w", name, ErrUsage)
		}
	} else {
		if len(name) == 0 {
			return 0, fmt.Errorf("bare '-' is not a valid flag: %w", ErrUsage)
		}
		// Combined short flags are ambiguous when a member accepts a value.
		r := []rune(name)[0]
		if len([]rune(name)) > 1 && !hasInline {
			return 0, fmt.Errorf("unknown flag -%s (combined short flags not supported): %w", name, ErrUsage)
		}
		f = findFlagShort(flags, r)
		if f == nil {
			return 0, fmt.Errorf("unknown flag -%s: %w", string(r), ErrUsage)
		}
	}

	if f.flagIsBool() {
		val := "true"
		if hasInline {
			val = valIn
		}
		return 0, f.flagApplyString(vs, val)
	}
	if hasInline {
		return 0, f.flagApplyString(vs, valIn)
	}
	if !haveNext {
		return 0, fmt.Errorf("--%s: missing value: %w", f.flagName(), ErrUsage)
	}
	return 1, f.flagApplyString(vs, next)
}

// bindPositionals assigns tokens in declaration order and gives the remainder to a variadic.
func bindPositionals(args []AnyArg, positional []string, vs values) error {
	idx := 0
	for _, a := range args {
		if a.argVariadic() {
			bound := 0
			for idx < len(positional) {
				if err := a.argApplyString(vs, positional[idx]); err != nil {
					return err
				}
				idx++
				bound++
			}
			if a.argRequired() && bound == 0 {
				return fmt.Errorf("missing argument <%s>: %w", a.argName(), ErrMissingArg)
			}
			break
		}
		if idx >= len(positional) {
			if a.argRequired() {
				return fmt.Errorf("missing argument <%s>: %w", a.argName(), ErrMissingArg)
			}
			continue
		}
		if err := a.argApplyString(vs, positional[idx]); err != nil {
			return err
		}
		idx++
	}
	if idx < len(positional) {
		return fmt.Errorf("unexpected argument %q: %w", positional[idx], ErrUsage)
	}
	return nil
}

func commandNames(path []*Command) []string {
	out := make([]string, len(path))
	for i, c := range path {
		out[i] = c.Name
	}
	return out
}

// unknownSubcommand includes a suggestion when a visible name is sufficiently close.
func unknownSubcommand(c *Command, attempted string) error {
	suggestion := suggestSub(c, attempted)
	if suggestion != "" {
		return fmt.Errorf("unknown subcommand %q (did you mean %q?): %w", attempted, suggestion, ErrUnknownCmd)
	}
	return fmt.Errorf("unknown subcommand %q: %w", attempted, ErrUnknownCmd)
}

// suggestSub returns the closest visible subcommand within the edit-distance limit.
func suggestSub(c *Command, attempted string) string {
	best := ""
	bestDist := len(attempted)
	if bestDist > 4 {
		bestDist = 4
	}
	for _, s := range c.Subcommands {
		if s.Hidden {
			continue
		}
		d := levenshtein(attempted, s.Name)
		if d < bestDist {
			best = s.Name
			bestDist = d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
