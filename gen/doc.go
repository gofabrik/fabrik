// Package gen defines the directive contract and render tree for main.gen.go.
//
// # Directive lifecycle
//
// A directive parses annotations, validates typed targets, and emits nodes.
// [Meta] declares syntax, docs, completions, and emission tier.
//
// Lazy binds register demands for constructed values. Shared callbacks use
// [Gen.SingletonAttribution], ordered sequences use [Gen.BeginBatch], and
// variables defined outside a bind path register their region-signature types
// with [Gen.DeclareVarType].
//
// # The generation tree
//
// Emit appends typed nodes; [Raw] holds preformatted lines, and [Base] records
// origin, phase, labels, and manual dependencies.
//
// # Lifecycle contract
//
// A flow is the work for one command; an app without commands has one default
// flow. Config sources resolve before config loads. Setup hooks run in
// declaration order after their injected configs load and before providers.
// Other configs load once, immediately before their first consumer.
//
// Phase order applies within each extracted region, not across a generated
// file. A connected construction group extracts only when at least two flows
// share it, it has multiple nodes and a content anchor, it stays contiguous in
// each flow, and its boundary is narrow. Other work remains inline; duplication
// across flows is safe because a process executes one flow. A commandless app
// renders its default flow as one phase-ordered run body.
//
// # Rendering policies
//
// Within a region, the renderer groups nodes by phase, keeps dependency
// clusters contiguous, orders independent clusters by source anchor, and emits
// deterministic Go.
package gen
