// Command nightly-report prints a consolidated Markdown report from per-module nightly check artifacts.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofabrik/fabrik/internal/tools/nightlyreport"
)

func main() {
	artifacts := flag.String("artifacts", "", "directory of downloaded per-module artifacts")
	modules := flag.String("modules", "", "comma-separated module list from discovery")
	freshness := flag.String("freshness", "", "path to `go list -m -u -json all` output")
	freshnessExit := flag.Int("freshness-exit", 0, "exit code of the freshness `go list`")
	budget := flag.Int("budget", 900_000, "maximum report size in bytes")
	toolchain := flag.String("toolchain", "", "toolchain label for the header")
	tools := flag.String("tools", "", "tool-versions label for the header")
	date := flag.String("date", "", "report date for the header")
	flag.Parse()

	var results []nightlyreport.ModuleResult
	for _, m := range strings.Split(*modules, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		results = append(results, classifyModule(filepath.Join(*artifacts, nightlyreport.Slug(m)), m))
	}

	freshStatus, updates := loadFreshness(*freshness, *freshnessExit)

	meta := nightlyreport.Meta{Date: *date, Toolchain: *toolchain, Tools: *tools}
	fmt.Print(nightlyreport.Render(results, freshStatus, updates, meta, *budget))
}

// loadFreshness treats an empty path as not run and read failures as errors.
func loadFreshness(path string, exit int) (nightlyreport.Status, []nightlyreport.Update) {
	if path == "" {
		return nightlyreport.StatusNotRun, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nightlyreport.StatusError, nil
	}
	updates, _ := nightlyreport.Freshness(data)
	return nightlyreport.ClassifyFreshness(data, exit), updates
}

// classifyModule treats an absent artifact directory as not run and missing check output as an error.
func classifyModule(dir, module string) nightlyreport.ModuleResult {
	ran := isDir(dir)

	lintJSON, lintJSONOK := readFile(dir, "lint.json")
	lintTxt, _ := readFile(dir, "lint.txt")
	lintOutcome, _ := readFile(dir, "lint.outcome")
	// With --issues-exit-code=0, a non-success outcome indicates a runner error.
	lintOK := lintJSONOK && strings.TrimSpace(string(lintOutcome)) == "success"
	lint := nightlyreport.Resolve(ran, lintOK, nightlyreport.ClassifyLint(lintJSON))

	vulnJSON, vulnJSONOK := readFile(dir, "vuln.json")
	vulnErr, _ := readFile(dir, "vuln.err")
	vulnExit, vulnExitOK := readExit(dir, "vuln.exit")
	vuln := nightlyreport.Resolve(ran, vulnJSONOK && vulnExitOK, nightlyreport.ClassifyVuln(vulnJSON, vulnExit))

	testJSON, testJSONOK := readFile(dir, "test.json")
	testErr, _ := readFile(dir, "test.err")
	testExit, testExitOK := readExit(dir, "test.exit")
	test := nightlyreport.Resolve(ran, testJSONOK && testExitOK, nightlyreport.ClassifyTest(testJSON, testExit))

	tidyTxt, tidyTxtOK := readFile(dir, "tidy.txt")
	tidyErr, _ := readFile(dir, "tidy.err")
	tidyExit, tidyExitOK := readExit(dir, "tidy.exit")
	tidy := nightlyreport.Resolve(ran, tidyTxtOK && tidyExitOK, nightlyreport.ClassifyTidy(tidyTxt, tidyErr, tidyExit))

	return nightlyreport.ModuleResult{
		Module:     module,
		Lint:       lint,
		Vuln:       vuln,
		Test:       test,
		Tidy:       tidy,
		LintDetail: string(lintTxt),
		VulnDetail: detailFor(vuln, nightlyreport.VulnSummary(vulnJSON), string(vulnErr)),
		TestDetail: detailFor(test, nightlyreport.TestOutput(testJSON), string(testErr)),
		TidyDetail: detailFor(tidy, string(tidyTxt), string(tidyErr)),
	}
}

// detailFor falls back to stderr when an errored tool has no normal output.
func detailFor(status nightlyreport.Status, normal, errText string) string {
	if status == nightlyreport.StatusError && strings.TrimSpace(normal) == "" {
		return errText
	}
	return normal
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func readFile(dir, name string) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, false
	}
	return data, true
}

func readExit(dir, name string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return n, true
}
