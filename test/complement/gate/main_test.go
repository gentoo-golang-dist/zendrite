package main

import (
	"bytes"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func mustList(t *testing.T, s string) List {
	t.Helper()
	list, err := ParseList(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func sampleResultMap(t *testing.T) map[string]Result {
	t.Helper()
	results := map[string]Result{}
	if err := ParseResults(strings.NewReader(sampleResults), results); err != nil {
		t.Fatal(err)
	}
	return results
}

func TestParseList(t *testing.T) {
	list := mustList(t, `
# comment
tests/TestACLs
tests/csapi/TestSync   trailing note

tests/csapi/TestLogin
`)
	if got, want := list.Packages(), []string{"tests", "tests/csapi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Packages() = %v, want %v", got, want)
	}
	if got, want := list.Tests("tests/csapi"), []string{"TestLogin", "TestSync"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tests() = %v, want %v", got, want)
	}
	if _, err := ParseList(strings.NewReader("tests/csapi/NotATest\n")); err == nil {
		t.Error("ParseList accepted a name that is not a test")
	}
}

func TestRunExpression(t *testing.T) {
	re := regexp.MustCompile(RunExpression([]string{"TestSync", "TestLogin"}))
	// go test matches -run against each level of a test name separately, so
	// the expression must match the top-level names exactly.
	for name, want := range map[string]bool{
		"TestSync":        true,
		"TestLogin":       true,
		"TestSyncFilter":  false,
		"TestSyncTimeout": false,
		"XTestSync":       false,
	} {
		if got := re.MatchString(name); got != want {
			t.Errorf("match %q = %v, want %v", name, got, want)
		}
	}
}

const sampleResults = `go: downloading something
{"Action":"run","Package":"github.com/matrix-org/complement/tests/csapi","Test":"TestSync"}
{"Action":"pass","Package":"github.com/matrix-org/complement/tests/csapi","Test":"TestSync/sub","Elapsed":1}
{"Action":"pass","Package":"github.com/matrix-org/complement/tests/csapi","Test":"TestSync","Elapsed":2}
{"Action":"fail","Package":"github.com/matrix-org/complement/tests/csapi","Test":"TestLogin","Elapsed":2}
{"Action":"fail","Package":"github.com/matrix-org/complement/tests/csapi","Test":"TestFlaky","Elapsed":2}
{"Action":"pass","Package":"github.com/matrix-org/complement/tests","Test":"TestNew","Elapsed":2}
{"Action":"fail","Package":"github.com/matrix-org/complement/tests","Test":"TestBroken","Elapsed":2}
{"Action":"fail","Package":"github.com/matrix-org/complement/tests/csapi","Elapsed":9}
`

func TestParseResults(t *testing.T) {
	got := sampleResultMap(t)
	want := map[string]Result{
		"tests/csapi/TestSync":  Pass,
		"tests/csapi/TestLogin": Fail,
		"tests/csapi/TestFlaky": Fail,
		"tests/TestNew":         Pass,
		"tests/TestBroken":      Fail,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseResults() = %v, want %v", got, want)
	}
}

func TestFailedRuns(t *testing.T) {
	list := mustList(t, "tests/csapi/TestSync\ntests/csapi/TestLogin\ntests/csapi/TestFlaky\ntests/msc1/TestMissing\n")
	got := FailedRuns(list, sampleResultMap(t))
	want := []string{
		`tests/csapi ^(TestFlaky|TestLogin)$`,
		`tests/msc1 ^(TestMissing)$`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FailedRuns() = %q, want %q", got, want)
	}
}

func TestCheck(t *testing.T) {
	list := mustList(t, "tests/csapi/TestSync\ntests/csapi/TestLogin\ntests/csapi/TestFlaky\ntests/msc1/TestMissing\n")
	retry := map[string]Result{
		"tests/csapi/TestLogin": Fail,
		"tests/csapi/TestFlaky": Pass,
	}
	r := Check(list, List{}, sampleResultMap(t), retry)
	want := Report{
		Failed:       []string{"tests/csapi/TestLogin"},
		Missing:      []string{"tests/msc1/TestMissing"},
		Flaky:        []string{"tests/csapi/TestFlaky"},
		Passed:       1,
		NewlyPassing: []string{"tests/TestNew"},
		Unlisted:     1,
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("Check() = %+v, want %+v", r, want)
	}
	if r.OK() {
		t.Error("OK() = true with failed and missing tests")
	}

	var out bytes.Buffer
	r.Write(&out, false)
	if strings.Contains(out.String(), "TestNew") {
		t.Errorf("a partial run reported newly passing tests:\n%s", out.String())
	}
	out.Reset()
	r.Write(&out, true)
	if !strings.Contains(out.String(), "TestNew") {
		t.Errorf("a full run did not report newly passing tests:\n%s", out.String())
	}

	if !Check(mustList(t, "tests/csapi/TestSync\n"), List{}, sampleResultMap(t), nil).OK() {
		t.Error("OK() = false although every listed test passed")
	}

	// Known flaky tests are neither suggested for the list nor counted.
	r = Check(mustList(t, "tests/csapi/TestSync\n"), mustList(t, "tests/TestNew\ntests/TestBroken\n"), sampleResultMap(t), nil)
	// Only TestLogin and TestFlaky are left: unlisted, not flaky, and failing.
	if len(r.NewlyPassing) != 0 || r.Unlisted != 2 {
		t.Errorf("flaky tests were reported: %+v", r)
	}
}
