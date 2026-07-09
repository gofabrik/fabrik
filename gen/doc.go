// Package gen defines the directive contract and render tree for main.gen.go.
//
// # Directive lifecycle
//
// A directive parses annotations, validates typed targets, and emits nodes.
// [Meta] declares syntax, docs, completions, and emission tier.
//
// # The generation tree
//
// Emit appends typed nodes such as [ConfigLoad], [Call], [StructLit],
// [Select], [Route], [Serve], and [Assign]. [Raw] holds preformatted lines
// when no typed node fits. [Base] records origin, phase, optional label, and
// manual dependencies.
//
// # Rendering policies
//
// The renderer groups nodes by phase, keeps dependency clusters contiguous,
// orders independent clusters by source anchor, and emits deterministic Go.
package gen
