// Package nightlyreport combines structured check output with captured exits to
// distinguish findings from masked tool failures, then renders Markdown reports.
package nightlyreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Status is the outcome of a module check.
type Status string

const (
	StatusClean    Status = "clean"
	StatusFindings Status = "findings"
	StatusError    Status = "error"
	StatusNotRun   Status = "not run"
)

// Resolve distinguishes jobs that did not run from checks with missing output.
func Resolve(ran, present bool, classified Status) Status {
	switch {
	case !ran:
		return StatusNotRun
	case !present:
		return StatusError
	default:
		return classified
	}
}

// ClassifyLint classifies golangci-lint JSON, treating malformed output and type-check failures as errors.
func ClassifyLint(data []byte) Status {
	var out struct {
		Issues *[]struct {
			FromLinter string `json:"FromLinter"`
		} `json:"Issues"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Issues == nil {
		return StatusError
	}
	for _, is := range *out.Issues {
		if is.FromLinter == "typecheck" {
			return StatusError
		}
	}
	if len(*out.Issues) > 0 {
		return StatusFindings
	}
	return StatusClean
}

// ClassifyVuln uses reachability for symbol scans, counts all coarser findings,
// and treats nonzero runner exits as errors.
func ClassifyVuln(data []byte, goRunExit int) Status {
	if goRunExit != 0 {
		return StatusError
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	symbol, validConfig := false, false
	anyFinding, reachable := false, false
	for {
		var msg struct {
			Config *struct {
				ScanLevel string `json:"scan_level"`
			} `json:"config"`
			Finding *struct {
				Trace []struct {
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return StatusError
		}
		if msg.Config != nil {
			switch msg.Config.ScanLevel {
			case "symbol", "package", "module":
				validConfig = true
				symbol = msg.Config.ScanLevel == "symbol"
			}
		}
		if msg.Finding != nil {
			anyFinding = true
			if len(msg.Finding.Trace) > 0 && msg.Finding.Trace[0].Function != "" {
				reachable = true
			}
		}
	}
	if !validConfig {
		return StatusError
	}
	if (symbol && reachable) || (!symbol && anyFinding) {
		return StatusFindings
	}
	return StatusClean
}

// ClassifyTest treats test failures as findings and build, setup, or runner failures as errors.
func ClassifyTest(data []byte, testExit int) Status {
	dec := json.NewDecoder(bytes.NewReader(data))
	failed := false
	for {
		var ev struct {
			Action      string `json:"Action"`
			FailedBuild string `json:"FailedBuild"`
		}
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				break
			}
			return StatusError
		}
		if ev.FailedBuild != "" {
			return StatusError
		}
		if ev.Action == "fail" {
			failed = true
		}
	}
	if failed {
		return StatusFindings
	}
	if testExit != 0 {
		return StatusError
	}
	return StatusClean
}

// ClassifyTidy classifies `go mod tidy -diff` output as clean, drift, or an error.
func ClassifyTidy(diff, stderr []byte, tidyExit int) Status {
	if tidyExit == 0 {
		return StatusClean
	}
	if len(bytes.TrimSpace(diff)) > 0 {
		return StatusFindings
	}
	return StatusError
}

// Update is a dependency with a newer version available.
type Update struct {
	Path, From, To string
}

// ClassifyFreshness treats available updates as findings and unusable output or a nonzero exit as errors.
func ClassifyFreshness(data []byte, listExit int) Status {
	if listExit != 0 {
		return StatusError
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	total, updates := 0, 0
	for {
		var m struct {
			Path   string
			Update *struct{ Version string }
		}
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return StatusError
		}
		if m.Path == "" {
			continue
		}
		total++
		if m.Update != nil {
			updates++
		}
	}
	// Empty output is invalid because go list always reports at least the main module.
	if total == 0 {
		return StatusError
	}
	if updates > 0 {
		return StatusFindings
	}
	return StatusClean
}

// Freshness returns unique modules with updates from `go list -m -u -json all` output.
func Freshness(data []byte) ([]Update, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var updates []Update
	seen := map[string]bool{}
	for {
		var m struct {
			Path    string
			Version string
			Update  *struct{ Version string }
		}
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if m.Update != nil && !seen[m.Path] {
			seen[m.Path] = true
			updates = append(updates, Update{Path: m.Path, From: m.Version, To: m.Update.Version})
		}
	}
	return updates, nil
}

// VulnSummary formats reachable vulnerabilities as one line per OSV and called symbol.
func VulnSummary(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	var lines []string
	seen := map[string]bool{}
	for {
		var msg struct {
			Finding *struct {
				OSV   string `json:"osv"`
				Trace []struct {
					Package  string `json:"package"`
					Receiver string `json:"receiver"`
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if err := dec.Decode(&msg); err != nil {
			break
		}
		f := msg.Finding
		if f == nil || len(f.Trace) == 0 || f.Trace[0].Function == "" || seen[f.OSV] {
			continue
		}
		seen[f.OSV] = true
		fr := f.Trace[0]
		sym := fr.Function
		if fr.Receiver != "" {
			sym = "(" + fr.Receiver + ")." + fr.Function
		}
		lines = append(lines, fmt.Sprintf("%s: %s.%s", f.OSV, fr.Package, sym))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestOutput joins Output fields from a `go test -json` event stream.
func TestOutput(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	for {
		var ev struct {
			Output string `json:"Output"`
		}
		if err := dec.Decode(&ev); err != nil {
			break
		}
		b.WriteString(ev.Output)
	}
	return b.String()
}

// Slug returns the artifact name produced by the workflow for a module path.
func Slug(module string) string {
	return strings.ReplaceAll(strings.TrimPrefix(module, "./"), "/", "-")
}

// ModuleResult contains one module's status and details for each check.
type ModuleResult struct {
	Module                                         string
	Lint, Vuln, Test, Tidy                         Status
	LintDetail, VulnDetail, TestDetail, TidyDetail string
}

// Meta holds report header facts.
type Meta struct {
	Date, Toolchain, Tools string
}

const truncNote = "\n_Truncated to the job-summary budget; full output is in the uploaded artifacts._\n"

// Render builds a Markdown report no larger than budget bytes, truncating details before summary content.
func Render(results []ModuleResult, freshness Status, updates []Update, meta Meta, budget int) string {
	var b strings.Builder
	b.WriteString("# Nightly checks report\n\n")
	if meta.Date != "" {
		fmt.Fprintf(&b, "Date: %s\n", meta.Date)
	}
	if meta.Toolchain != "" {
		fmt.Fprintf(&b, "Toolchain: %s\n", meta.Toolchain)
	}
	if meta.Tools != "" {
		fmt.Fprintf(&b, "Tools: %s\n", meta.Tools)
	}
	fmt.Fprintf(&b, "\nModules: %d\n\n", len(results))

	b.WriteString("## Summary\n\n| Module | Lint | Vulnerabilities | Tests | Dependencies |\n|---|---|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			r.Module, cell(r.Lint), cell(r.Vuln), cell(r.Test), cell(r.Tidy))
	}
	fmt.Fprintf(&b, "\n**Dependency freshness (workspace):** %s\n\n", cell(freshness))
	switch {
	case len(updates) > 0:
		for _, u := range updates {
			fmt.Fprintf(&b, "- `%s` %s -> %s\n", u.Path, u.From, u.To)
		}
	case freshness == StatusClean:
		b.WriteString("No available updates.\n")
	default:
		b.WriteString("Freshness result unavailable.\n")
	}

	b.WriteString("\n## Details\n")

	if b.Len()+len(truncNote) > budget {
		keep := budget - len(truncNote)
		if keep < 0 {
			keep = 0
		}
		return clamp(b.String()[:keep]+truncNote, budget)
	}

	// Reserve space for the truncation note in every detail block.
	truncated := false
	for _, r := range results {
		for _, d := range details(r) {
			if d.status != StatusFindings && d.status != StatusError {
				continue
			}
			block, full := detailBlock(r.Module, d.check, d.status, d.detail, budget-b.Len()-len(truncNote))
			b.WriteString(block)
			if !full {
				truncated = true
				break
			}
		}
		if truncated {
			break
		}
	}
	out := b.String()
	if truncated {
		out += truncNote
	}
	return clamp(out, budget)
}

func clamp(s string, budget int) string {
	if budget < 0 {
		budget = 0
	}
	if len(s) > budget {
		return s[:budget]
	}
	return s
}

type detail struct {
	check, detail string
	status        Status
}

func details(r ModuleResult) []detail {
	return []detail{
		{"lint", r.LintDetail, r.Lint},
		{"vulnerabilities", r.VulnDetail, r.Vuln},
		{"tests", r.TestDetail, r.Test},
		{"dependencies", r.TidyDetail, r.Tidy},
	}
}

// detailBlock reports full as false when the block is truncated or omitted.
func detailBlock(module, check string, status Status, body string, remaining int) (block string, full bool) {
	head := fmt.Sprintf("\n<details><summary><code>%s</code> - %s (%s)</summary>\n\n```\n", module, check, status)
	tail := "\n```\n\n</details>\n"
	overhead := len(head) + len(tail)
	if remaining <= 0 || overhead+len("\n(truncated)") > remaining {
		return "", false
	}
	if len(body)+overhead <= remaining {
		return head + body + tail, true
	}
	cut := remaining - overhead - len("\n(truncated)")
	if cut < 0 {
		cut = 0
	}
	return head + body[:cut] + "\n(truncated)" + tail, false
}

func cell(s Status) string {
	switch s {
	case StatusClean:
		return "clean"
	case StatusFindings:
		return "**findings**"
	case StatusError:
		return "`error`"
	case StatusNotRun:
		return "not run"
	default:
		return string(s) // Preserve unexpected values.
	}
}
