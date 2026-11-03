// Command gate turns Complement's `go test -json` output into a CI verdict.
//
// Zendrite does not pass every Complement test, so CI gates on a list of
// tests that are known to pass (test/complement/passing.txt). A listed test
// that fails, or does not run at all, fails the job. Tests that are not on
// the list are reported but never fail it, so a run of the whole
// suite can point out tests that have started to pass.
//
// Usage:
//
//	gate packages -list passing.txt
//	gate regex -list passing.txt -pkg tests/csapi
//	gate failed -list passing.txt results.json...
//	gate check -list passing.txt [-flaky flaky.txt] [-retry retry.json] [-full] results.json...
//
// Tests in the flaky file are neither gated nor suggested for the list.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// modulePrefix is stripped from package paths in `go test -json` output so
// tests read as they do in the passing list, for example tests/csapi/TestSync.
const modulePrefix = "github.com/matrix-org/complement/"

const (
	// ExitUsage is the exit code for usage and input errors, as opposed to 1
	// for a failed check.
	exitUsage = 2
	// MaxLineSize bounds one line of `go test -json` output; Complement logs
	// whole homeserver responses.
	maxLineSize     = 16 << 20
	initialLineSize = 1 << 20
)

// Result is the outcome of one top-level test.
type Result string

const (
	Pass Result = "pass"
	Fail Result = "fail"
	Skip Result = "skip"
)

// event is the subset of a `go test -json` line that the gate reads.
type event struct {
	Action  string
	Package string
	Test    string
}

func main() {
	if len(os.Args) < 2 { //nolint:mnd // the subcommand
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	listPath := fs.String("list", "test/complement/passing.txt", "file listing the tests that must pass")
	pkg := fs.String("pkg", "", "package to build a -run expression for (regex)")
	retryPath := fs.String("retry", "", "go test -json output of the retried tests (check)")
	flakyPath := fs.String("flaky", "", "file listing known flaky tests, in the same format (check)")
	full := fs.Bool("full", false, "the run covered the whole suite, so report tests that newly pass (check)")
	_ = fs.Parse(args)

	list, err := readList(*listPath)
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "packages":
		for _, p := range list.Packages() {
			fmt.Println(p)
		}
	case "regex":
		if *pkg == "" {
			fatal(errors.New("regex needs -pkg"))
		}
		fmt.Println(RunExpression(list.Tests(*pkg)))
	case "failed":
		results, err := readResultFiles(fs.Args())
		if err != nil {
			fatal(err)
		}
		for _, line := range FailedRuns(list, results) {
			fmt.Println(line)
		}
	case "check":
		results, err := readResultFiles(fs.Args())
		if err != nil {
			fatal(err)
		}
		var retry map[string]Result
		if *retryPath != "" {
			if retry, err = readResultFiles([]string{*retryPath}); err != nil {
				fatal(err)
			}
		}
		flaky := List{}
		if *flakyPath != "" {
			if flaky, err = readList(*flakyPath); err != nil {
				fatal(err)
			}
		}
		report := Check(list, flaky, results, retry)
		report.Write(os.Stdout, *full)
		if !report.OK() {
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gate packages|regex|failed|check [flags] [results.json...]")
	os.Exit(exitUsage)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gate:", err)
	os.Exit(exitUsage)
}

// List is the set of tests that must pass, keyed as <package>/<TestName>.
type List map[string]bool

func readList(path string) (List, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseList(f)
}

// ParseList reads one test per line. Blank lines and lines starting with #
// are ignored, as is anything after the test name.
func ParseList(r io.Reader) (List, error) {
	list := List{}
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.Fields(line)[0]
		if _, test := splitTest(name); !strings.HasPrefix(test, "Test") {
			return nil, fmt.Errorf("line %d: %q is not <package>/<TestName>", n, name)
		}
		list[name] = true
	}
	return list, scanner.Err()
}

// Packages returns the packages that have listed tests, sorted.
func (l List) Packages() []string {
	seen := map[string]bool{}
	for name := range l {
		pkg, _ := splitTest(name)
		seen[pkg] = true
	}
	return sortedKeys(seen)
}

// Tests returns the listed test names in pkg, sorted.
func (l List) Tests(pkg string) []string {
	var tests []string
	for name := range l {
		if p, test := splitTest(name); p == pkg {
			tests = append(tests, test)
		}
	}
	sort.Strings(tests)
	return tests
}

// RunExpression returns a `go test -run` expression matching exactly the
// given top-level tests and all of their subtests.
func RunExpression(tests []string) string {
	quoted := make([]string, len(tests))
	for i, t := range tests {
		quoted[i] = regexp.QuoteMeta(t)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

func splitTest(name string) (pkg, test string) {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return "", name
	}
	return name[:i], name[i+1:]
}

func readResultFiles(paths []string) (map[string]Result, error) {
	results := map[string]Result{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		err = ParseResults(f, results)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return results, nil
}

// ParseResults records the outcome of every top-level test in a `go test
// -json` stream into results. Lines that are not JSON, such as build output,
// are ignored. A test that ran more than once keeps its last outcome.
func ParseResults(r io.Reader, results map[string]Result) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, initialLineSize), maxLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Test == "" || strings.Contains(e.Test, "/") {
			continue
		}
		switch Result(e.Action) {
		case Pass, Fail, Skip:
			results[strings.TrimPrefix(e.Package, modulePrefix)+"/"+e.Test] = Result(e.Action)
		}
	}
	return scanner.Err()
}

// FailedRuns returns one "<package> <-run expression>" line per package with
// listed tests that did not pass, for retrying them.
func FailedRuns(list List, results map[string]Result) []string {
	failed := List{}
	for name := range list {
		if results[name] != Pass {
			failed[name] = true
		}
	}
	var lines []string
	for _, pkg := range failed.Packages() {
		lines = append(lines, pkg+" "+RunExpression(failed.Tests(pkg)))
	}
	return lines
}

// Report sorts every test into the categories the job prints.
type Report struct {
	// Failed are listed tests that did not pass, even on retry.
	Failed []string
	// Missing are listed tests that never ran.
	Missing []string
	// Flaky are listed tests that failed once and passed on retry.
	Flaky []string
	// Passed counts the listed tests that passed.
	Passed int
	// NewlyPassing are tests that passed but are not listed yet.
	NewlyPassing []string
	// Unlisted counts the tests that ran but are not listed and did not pass.
	Unlisted int
}

// Check compares results, and the results of retried tests, with the list.
// Known flaky tests are left out of the unlisted tests.
func Check(list, flaky List, results, retry map[string]Result) Report {
	var r Report
	for _, name := range sortedKeys(list) {
		res, ran := results[name]
		switch {
		case res == Pass:
			r.Passed++
		case retry[name] == Pass:
			r.Flaky = append(r.Flaky, name)
		case !ran && retry[name] == "":
			r.Missing = append(r.Missing, name)
		default:
			r.Failed = append(r.Failed, name)
		}
	}
	for _, name := range sortedKeys(results) {
		if list[name] || flaky[name] {
			continue
		}
		if results[name] == Pass {
			r.NewlyPassing = append(r.NewlyPassing, name)
		} else {
			r.Unlisted++
		}
	}
	return r
}

// OK reports whether every listed test passed, possibly on retry.
func (r Report) OK() bool {
	return len(r.Failed) == 0 && len(r.Missing) == 0
}

// Write prints the report. Newly passing tests are only meaningful when the
// whole suite ran, so they are printed only for full runs.
func (r Report) Write(w io.Writer, full bool) {
	fmt.Fprintf(w, "Listed tests: %d passed, %d flaky, %d failed, %d did not run.\n",
		r.Passed, len(r.Flaky), len(r.Failed), len(r.Missing))
	section(w, "Listed tests that failed", r.Failed)
	section(w, "Listed tests that did not run", r.Missing)
	section(w, "Listed tests that failed once and passed on retry", r.Flaky)
	if full {
		fmt.Fprintf(w, "Unlisted tests that did not pass: %d.\n", r.Unlisted)
		section(w, "Unlisted tests that passed (add them to test/complement/passing.txt)", r.NewlyPassing)
	}
}

func section(w io.Writer, title string, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, n := range names {
		fmt.Fprintf(w, "  %s\n", n)
	}
	fmt.Fprintln(w)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
