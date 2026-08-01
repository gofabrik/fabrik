// Package genconfig resolves project generation settings from fabrik.yaml and invocation overrides.
package genconfig

import (
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"unicode"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	"gopkg.in/yaml.v3"
)

// Emit selects where generated output goes.
type Emit int

const (
	EmitStandalone Emit = iota
	EmitEmbedded
)

// Split selects how generated output is divided into files.
type Split int

const (
	SplitOff Split = iota
	SplitFragment
)

// Options is the resolved generation configuration for one invocation.
type Options struct {
	Emit        Emit
	Dir         string
	Package     string
	Split       Split
	BuildTag    string
	Entrypoints []string
	Comments    gen.CommentLevel

	// Positions anchor engine diagnostics in fabrik.yaml.
	EmitPos       token.Position
	DirPos        token.Position
	EntrypointPos map[string]token.Position
}

// Overrides contains invocation settings that take precedence over fabrik.yaml.
type Overrides struct {
	Comments *gen.CommentLevel
}

const fileName = "fabrik.yaml"

// Resolve finds fabrik.yaml beside go.mod and applies ov over its settings.
// A module without fabrik.yaml uses the zero-value defaults.
func Resolve(dir string, ov Overrides) (Options, diag.Diagnostics) {
	return ResolveOverlay(dir, nil, ov)
}

// ResolveOverlay is Resolve with unsaved contents overriding fabrik.yaml on disk.
func ResolveOverlay(dir string, overlay map[string][]byte, ov Overrides) (Options, diag.Diagnostics) {
	opts := defaults()

	root, ok := findModuleRoot(dir)
	if !ok {
		return applyOverrides(opts, ov), nil
	}
	path := filepath.Join(root, fileName)
	if data, ok := overlay[path]; ok {
		opts, diags := parseFile(path, data, opts)
		return applyOverrides(opts, ov), diags
	}
	data, err := os.ReadFile(path) // #nosec G304 -- reads the module-relative project file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return applyOverrides(opts, ov), nil
		}
		var diags diag.Diagnostics
		diags.Error(token.Position{Filename: path}, fmt.Sprintf("read %s: %v", fileName, err), "")
		return applyOverrides(opts, ov), diags
	}

	opts, diags := parseFile(path, data, opts)
	return applyOverrides(opts, ov), diags
}

func defaults() Options {
	return Options{Emit: EmitStandalone, Split: SplitOff, Comments: gen.CommentsSections}
}

func applyOverrides(opts Options, ov Overrides) Options {
	if ov.Comments != nil {
		opts.Comments = *ov.Comments
	}
	return opts
}

func findModuleRoot(dir string) (string, bool) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

func parseFile(path string, data []byte, opts Options) (Options, diag.Diagnostics) {
	var diags diag.Diagnostics
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		diags.Error(token.Position{Filename: path, Line: yamlErrorLine(err)},
			fmt.Sprintf("malformed %s: %v", fileName, err), "")
		return opts, diags
	}
	if len(doc.Content) == 0 {
		return opts, diags
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		diags.Error(posAt(path, root), fmt.Sprintf("%s must be a mapping", fileName), "")
		return opts, diags
	}

	var generate *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Value == "generate" {
			if generate != nil {
				diags.Error(posAt(path, key), `duplicate key "generate"`, "")
				continue
			}
			generate = root.Content[i+1]
			continue
		}
		diags.Error(posAt(path, key), fmt.Sprintf("unknown key %q", key.Value), "known: generate")
	}
	if generate == nil {
		return opts, diags
	}
	gd := resolveGenerate(path, generate, &opts)
	diags = append(diags, gd...)
	return opts, diags
}

func resolveGenerate(path string, node *yaml.Node, opts *Options) diag.Diagnostics {
	var diags diag.Diagnostics
	if node.Kind != yaml.MappingNode {
		diags.Error(posAt(path, node), "generate must be a mapping", "")
		return diags
	}

	var dirNode, packageNode, emitNode, entrypointsKey *yaml.Node
	var dirSeen, emitInvalid bool
	scalar := func(key, val *yaml.Node) bool {
		if val.Kind == yaml.ScalarNode && val.Tag != "!!null" && val.Value != "" {
			return true
		}
		diags.Error(posAt(path, val), fmt.Sprintf("%s must be a non-empty value", key.Value), "")
		return false
	}
	seenKeys := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if seenKeys[key.Value] {
			diags.Error(posAt(path, key), fmt.Sprintf("duplicate key %q", key.Value), "")
			continue
		}
		seenKeys[key.Value] = true
		switch key.Value {
		case "emit":
			if !scalar(key, val) {
				emitInvalid = true
				continue
			}
			emitNode = val
			opts.EmitPos = posAt(path, val)
			switch val.Value {
			case "standalone":
				opts.Emit = EmitStandalone
			case "embedded":
				opts.Emit = EmitEmbedded
			default:
				emitInvalid = true
				diags.Error(posAt(path, val), fmt.Sprintf("invalid emit %q", val.Value), "want standalone or embedded")
			}
		case "dir":
			dirSeen = true
			if !scalar(key, val) {
				continue
			}
			dirNode = val
			opts.DirPos = posAt(path, val)
			if !filepath.IsLocal(val.Value) {
				diags.Error(posAt(path, val), fmt.Sprintf("dir %q must stay inside the module", val.Value),
					"use a relative path without .. segments")
				continue
			}
			opts.Dir = val.Value
		case "package":
			if !scalar(key, val) {
				continue
			}
			packageNode = val
			opts.Package = val.Value
		case "split":
			if !scalar(key, val) {
				continue
			}
			switch val.Value {
			case "off":
				opts.Split = SplitOff
			case "fragment":
				opts.Split = SplitFragment
			default:
				diags.Error(posAt(path, val), fmt.Sprintf("invalid split %q", val.Value), "want off or fragment")
			}
		case "buildtag":
			if !scalar(key, val) {
				continue
			}
			if !validBuildTag(val.Value) {
				diags.Error(posAt(path, val), fmt.Sprintf("invalid buildtag %q", val.Value),
					"a tag is letters, digits, underscores, and dots")
				continue
			}
			opts.BuildTag = val.Value
		case "entrypoints":
			entrypointsKey = key
			if val.Kind != yaml.SequenceNode {
				diags.Error(posAt(path, val), "entrypoints must be a list", "")
				break
			}
			seen := map[string]bool{}
			for _, item := range val.Content {
				if item.Kind != yaml.ScalarNode || item.Tag == "!!null" || item.Value == "" {
					diags.Error(posAt(path, item), "entrypoints entries must be command paths", "")
					continue
				}
				if seen[item.Value] {
					diags.Error(posAt(path, item), fmt.Sprintf("duplicate entrypoint %q", item.Value), "")
					continue
				}
				seen[item.Value] = true
				opts.Entrypoints = append(opts.Entrypoints, item.Value)
				if opts.EntrypointPos == nil {
					opts.EntrypointPos = map[string]token.Position{}
				}
				opts.EntrypointPos[item.Value] = posAt(path, item)
			}
		case "comments":
			if !scalar(key, val) {
				continue
			}
			lvl, err := ParseCommentLevel(val.Value)
			if err != nil {
				diags.Error(posAt(path, val), err.Error(), "want off, sections, or full")
				break
			}
			opts.Comments = lvl
		default:
			diags.Error(posAt(path, key), fmt.Sprintf("unknown key %q", key.Value),
				"known: emit, dir, package, split, buildtag, entrypoints, comments")
		}
	}

	if opts.Package != "" && !isValidPackageName(opts.Package) {
		diags.Error(posAt(path, packageNode), fmt.Sprintf("package %q is not a valid Go identifier", opts.Package), "")
	} else if opts.Dir != "" && opts.Package == "" {
		base := filepath.Base(opts.Dir)
		if isValidPackageName(base) {
			opts.Package = base
		} else {
			diags.Error(posAt(path, dirNode), fmt.Sprintf("package required: dir base %q is not a valid Go identifier", base), "set package explicitly")
		}
	}

	// A rejected emit or dir already carries its own diagnostic; checks
	// that depend on the missing value would only compound it.
	if opts.Emit == EmitEmbedded && opts.Dir == "" && !dirSeen {
		diags.Error(posAt(path, emitNode), "emit: embedded requires dir", "set generate.dir to the output package directory")
	}
	if entrypointsKey != nil && opts.Emit != EmitEmbedded && !emitInvalid {
		diags.Error(posAt(path, entrypointsKey), "entrypoints requires emit: embedded", "")
	}
	if opts.Emit != EmitEmbedded && !emitInvalid {
		if dirNode != nil {
			diags.Warn(posAt(path, dirNode), "dir is ignored unless emit: embedded", "")
		}
		if packageNode != nil {
			diags.Warn(posAt(path, packageNode), "package is ignored unless emit: embedded", "")
		}
	}

	return diags
}

// ParseCommentLevel parses a comments setting or flag value.
func ParseCommentLevel(s string) (gen.CommentLevel, error) {
	switch s {
	case "", "sections":
		return gen.CommentsSections, nil
	case "off":
		return gen.CommentsOff, nil
	case "full":
		return gen.CommentsFull, nil
	}
	return 0, fmt.Errorf("invalid comments level %q", s)
}

// yamlErrorLine recovers the line a yaml parse error reports in its
// message; the library exposes no structured position for it.
func yamlErrorLine(err error) int {
	m := yamlLinePattern.FindStringSubmatch(err.Error())
	if len(m) != 2 {
		return 0
	}
	n, cerr := strconv.Atoi(m[1])
	if cerr != nil {
		return 0
	}
	return n
}

var yamlLinePattern = regexp.MustCompile(`line (\d+):`)

func validBuildTag(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func isValidPackageName(s string) bool {
	return token.IsIdentifier(s) && !token.IsKeyword(s)
}

func posAt(path string, n *yaml.Node) token.Position {
	if n == nil {
		return token.Position{Filename: path}
	}
	return token.Position{Filename: path, Line: n.Line, Column: n.Column}
}
