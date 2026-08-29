package adr

// This file checks docs/adr against the invariants its own README states
// but nothing enforced: numbers are unique and contiguous, every file is
// listed in the README's table (and vice versa), every reference to an ADR
// anywhere in the repo points at a file that exists, a superseded record
// and its successor link to each other, and each file's title agrees with
// its filename. See issue #79 item 4 for the incident that motivated this:
// two branches each added a different docs/adr/0012-*.md. Different
// filenames, so git merged both without conflict, and main would have
// silently carried two ADR 0012s -- which docs/adr/README.md's "Adding a
// record" section explicitly forbids. Nothing caught it because nothing
// treated the ADR trail as a set of relationships that must hold, the same
// gap TestRoutesMatchSpec (internal/api/spec_test.go) closed for the OpenAPI
// spec and store.RunConformance closed for backend semantics.
//
// Why this package and not docs/adr itself: docs/adr is not a Go package
// (it's markdown, not source), and a _test.go file with no package to test
// still needs a package to live in. internal/api/spec_test.go's checks
// belong to package api because they're about that package's routes and
// types; these checks aren't about any one Go package, they're about the
// repo as a whole, so a small dedicated package next to the others under
// internal/ was chosen over bolting them onto an unrelated package (which
// would make a failure here look like it belongs to that package's
// behaviour) or a top-level script outside `go test` (which would fall
// outside `go test -race ./...` and stop running with everything else).
//
// Why not assert content correctness (e.g. that a superseded ADR's
// reasoning is actually obsolete): that's a judgment call a human makes
// when writing the record, not a fact a test can check. What follows is
// only the structural contract: the numbering, the cross-links, and the
// references resolve.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	adrDir   = "../../docs/adr"
	repoRoot = "../.."
)

// adrFilenameRE matches the "NNNN-slug.md" convention docs/adr/README.md's
// "Adding a record" section describes. The number is captured with its
// leading zeros intact so it can be compared as a string against text
// elsewhere (a title line, a table cell) without a leading-zero mismatch
// (e.g. "5" vs "0005") silently comparing equal after atoi.
var adrFilenameRE = regexp.MustCompile(`^(\d{4})-.+\.md$`)

// adrTitleRE matches the required first line of an ADR file, "# ADR NNNN: ...".
var adrTitleRE = regexp.MustCompile(`^# ADR (\d{4}): `)

// adrReferenceRE matches an in-text reference to an ADR, e.g. "ADR 0013" or
// "[ADR 0014]". It requires the literal "ADR " prefix specifically so it
// does not match a bare 4-digit run -- internal/metrics/metrics_test.go
// contains the float literal 0.0012, which must NOT be treated as a
// reference to ADR 0012. See TestADRReferenceRegexpIgnoresNumericLiterals,
// which pins that directly.
var adrReferenceRE = regexp.MustCompile(`\bADR (\d{4})\b`)

// supersededByRE and supersedesRE match the two halves of a supersession
// link. Both require the capitalized, bracketed form docs/adr's existing
// records use ("Superseded by [ADR 0014]", "Supersedes [ADR 0005]") so that
// prose which happens to use the word "supersedes" without linking an ADR
// -- e.g. docs/adr/0012-*.md's "supersedes the old
// `TestFingerprintOneAllNonTerminalIs404`", about a Go test, not an ADR --
// is not mistaken for a supersession link.
var (
	supersededByRE = regexp.MustCompile(`Superseded by \[ADR (\d{4})\]`)
	supersedesRE   = regexp.MustCompile(`Supersedes \[ADR (\d{4})\]`)
)

type adrFile struct {
	number   string // 4-digit, zero-padded, as it appears in the filename
	filename string // e.g. "0014-python-client-writes-running-then-patches-terminal.md"
	path     string // e.g. "../../docs/adr/0014-....md"
}

// listADRFiles returns every record under docs/adr, in filename order. It
// fails the test outright (not just reports an error) if the directory is
// missing or a file there doesn't match the naming convention, since every
// other test in this file assumes that much already holds.
func listADRFiles(t *testing.T) []adrFile {
	t.Helper()
	entries, err := os.ReadDir(adrDir)
	if err != nil {
		t.Fatalf("read %s: %v", adrDir, err)
	}
	var files []adrFile
	for _, e := range entries {
		if e.IsDir() || e.Name() == "README.md" {
			continue
		}
		m := adrFilenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			t.Fatalf("%s/%s does not follow the \"NNNN-slug.md\" naming convention docs/adr/README.md's \"Adding a record\" section describes -- rename it (or if it's not meant to be an ADR, move it out of docs/adr).", adrDir, e.Name())
			continue
		}
		files = append(files, adrFile{
			number:   m[1],
			filename: e.Name(),
			path:     filepath.Join(adrDir, e.Name()),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].filename < files[j].filename })
	return files
}

// TestADRNumbersAreUniqueAndContiguous is what would have caught the
// incident this test file exists for: two branches each added a
// docs/adr/0012-*.md, different filenames, so git merged both without a
// conflict. A duplicate number is an error on its own (see
// docs/adr/README.md: "never reuse or renumber"); a gap is a separate,
// milder problem (a record was deleted or renumbered by hand) but is
// checked here for the same reason -- both mean the sequence in
// docs/adr/README.md's table no longer describes what's on disk.
func TestADRNumbersAreUniqueAndContiguous(t *testing.T) {
	files := listADRFiles(t)

	byNumber := map[string][]string{}
	for _, f := range files {
		byNumber[f.number] = append(byNumber[f.number], f.filename)
	}

	var duplicated bool
	for n, names := range byNumber {
		if len(names) < 2 {
			continue
		}
		duplicated = true
		sort.Strings(names)
		t.Errorf("ADR %s is used by more than one file (%s) -- renumber all but one of them to the next free number; ADR numbers are never reused, per docs/adr/README.md's \"Adding a record\".", n, strings.Join(names, ", "))
	}
	if duplicated {
		// A duplicate number makes "what's the next expected number"
		// ambiguous, so the contiguity check below would only produce
		// confusing follow-on errors. The duplicate is already reported;
		// stop here rather than pile on.
		return
	}

	numbers := make([]int, 0, len(byNumber))
	for n := range byNumber {
		v, err := strconv.Atoi(n)
		if err != nil {
			t.Fatalf("ADR number %q is not numeric, but passed the filename pattern -- this is a bug in adrFilenameRE, not in docs/adr.", n)
		}
		numbers = append(numbers, v)
	}
	sort.Ints(numbers)
	for i, n := range numbers {
		want := i + 1
		if n != want {
			t.Errorf("ADR numbering has a gap: expected %04d to come next but found %04d -- ADR numbers must be contiguous starting from 0001 with no gaps (add the missing record, or if one was deleted, renumber the records after it down by one so the sequence closes up).", want, n)
			break
		}
	}
}

// adrReadmeRowRE matches one data row of docs/adr/README.md's "Records"
// table: "| [0014](0014-....md) | Title text | Status |". Only the first
// two cells are captured; title and status aren't part of this test's
// contract.
var adrReadmeRowRE = regexp.MustCompile(`^\| \[(\d{4})\]\(([^)]+)\) \|`)

// TestADRReadmeTableMatchesFiles checks docs/adr/README.md's table against
// the files on disk in both directions: every file has a row, every row's
// link resolves to a file that exists, and a row's link text (the number in
// brackets) agrees with its link target's filename (a row could otherwise
// have its number typo'd while still linking a real, different-numbered
// file -- README.md would render fine and still be wrong).
func TestADRReadmeTableMatchesFiles(t *testing.T) {
	files := listADRFiles(t)
	onDisk := map[string]bool{}
	for _, f := range files {
		onDisk[f.filename] = true
	}

	readmePath := filepath.Join(adrDir, "README.md")
	raw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}

	inTable := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		m := adrReadmeRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		linkNumber, target := m[1], m[2]
		inTable[target] = true

		fm := adrFilenameRE.FindStringSubmatch(target)
		if fm == nil {
			t.Errorf("%s:%d: table row links %q, which doesn't follow the \"NNNN-slug.md\" naming convention -- fix the link target.", readmePath, i+1, target)
			continue
		}
		if fm[1] != linkNumber {
			t.Errorf("%s:%d: table row's link text says ADR %s but links to %s (ADR %s) -- the two must agree; fix whichever one is wrong.", readmePath, i+1, linkNumber, target, fm[1])
		}
		if !onDisk[target] {
			t.Errorf("%s:%d: table row links to %s/%s, which does not exist -- fix the row or restore the file.", readmePath, i+1, adrDir, target)
		}
	}

	for _, f := range files {
		if !inTable[f.filename] {
			t.Errorf("%s/%s exists but has no row in %s's table -- add one (copy the format of an existing row, per \"Adding a record\").", adrDir, f.filename, readmePath)
		}
	}
}

// TestADRTitleMatchesFilename checks that each file's own claim about its
// number (the "# ADR NNNN: ..." title line) agrees with the number in its
// filename. The two are written independently by whoever created the
// record, so nothing stops them from drifting apart except a test.
func TestADRTitleMatchesFilename(t *testing.T) {
	for _, f := range listADRFiles(t) {
		t.Run(f.filename, func(t *testing.T) {
			raw, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read %s: %v", f.path, err)
			}
			firstLine, _, _ := strings.Cut(string(raw), "\n")
			m := adrTitleRE.FindStringSubmatch(firstLine)
			if m == nil {
				t.Fatalf("%s's first line must read \"# ADR %s: <title>\", got %q -- fix the title line.", f.path, f.number, firstLine)
			}
			if m[1] != f.number {
				t.Errorf("%s's title line claims ADR %s but the filename says ADR %s -- rename the file or fix the title, whichever is wrong.", f.path, m[1], f.number)
			}
		})
	}
}

// TestADRSupersessionLinksBack checks that every "Superseded by [ADR NNNN]"
// marker has a matching "Supersedes [ADR MMMM]" in the ADR it points to,
// and vice versa -- a one-directional link would let a reader land on the
// superseded record without a way to find out it was superseded, or land on
// the superseding one without knowing what it replaced.
//
// On current main the only such pair is ADR 0005 (superseded by ADR 0014).
// That pair is asserted explicitly, in addition to the general round-trip
// check below, so that if the marker were ever accidentally deleted from
// ADR 0005 (leaving both files individually well-formed, just no longer
// linked) this test would still fail instead of vacuously passing because
// there was nothing left to check.
func TestADRSupersessionLinksBack(t *testing.T) {
	files := listADRFiles(t)

	supersededBy := map[string]string{} // old ADR number -> new ADR number, from the old file
	supersedes := map[string]string{}   // new ADR number -> old ADR number, from the new file

	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		content := string(raw)
		if m := supersededByRE.FindStringSubmatch(content); m != nil {
			supersededBy[f.number] = m[1]
		}
		if m := supersedesRE.FindStringSubmatch(content); m != nil {
			supersedes[f.number] = m[1]
		}
	}

	for oldNum, newNum := range supersededBy {
		if got, ok := supersedes[newNum]; !ok || got != oldNum {
			t.Errorf("ADR %s says it is \"Superseded by [ADR %s]\", but ADR %s does not link back with \"Supersedes [ADR %s]\" -- add the back-link to the superseding record so a reader who lands there can find what it replaced.", oldNum, newNum, newNum, oldNum)
		}
	}
	for newNum, oldNum := range supersedes {
		if got, ok := supersededBy[oldNum]; !ok || got != newNum {
			t.Errorf("ADR %s says it \"Supersedes [ADR %s]\", but ADR %s is not marked \"Superseded by [ADR %s]\" -- add the marker to the superseded record so a reader who lands there is warned it was reversed.", newNum, oldNum, oldNum, newNum)
		}
	}

	if got := supersededBy["0005"]; got != "0014" {
		t.Errorf(`expected ADR 0005 to carry "Superseded by [ADR 0014]" (the known live case on main -- see issue #79 item 4), got superseded-by %q. If ADR 0005 was legitimately un-superseded or renumbered, update this expectation in the same change that edits the ADR files.`, got)
	}
}

// scannableExtensions are the file types issue #79 item 4 names as places
// an "ADR NNNN" reference can appear: Go comments, Python docstrings, and
// Markdown. docs/openapi.yaml is included too -- it cites ADR 0011 and
// ADR 0015 in its field descriptions -- since those are prose comments in a
// YAML file for the same reason a Go comment is prose in a .go file.
var scannableExtensions = map[string]bool{
	".go":   true,
	".py":   true,
	".md":   true,
	".yaml": true,
	".yml":  true,
}

// skipDirs are directories walked past without descending into: version
// control metadata, build output, and interpreter caches. None of these
// hold source files that could carry an ADR reference worth checking, and
// .git in particular is not always a directory (in a git worktree, as this
// checkout is, it's a file pointing elsewhere) so it must be checked before
// assuming it can be skipped as one.
var skipDirs = map[string]bool{
	".git":        true,
	".github":     true,
	"bin":         true,
	"__pycache__": true,
}

// TestADRReferencesResolve is the check named directly in issue #79 item 4:
// every "ADR NNNN" reference anywhere in the repo -- not just in
// docs/adr/README.md's table, which the tests above already cover -- must
// resolve to a real record. This is what would catch a reference to an ADR
// that was renumbered, or a typo'd reference that was never caught because
// nothing renders docs/adr/*.md and cross-checks it against prose scattered
// across the repo.
func TestADRReferencesResolve(t *testing.T) {
	files := listADRFiles(t)
	valid := map[string]bool{}
	for _, f := range files {
		valid[f.number] = true
	}

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasSuffix(d.Name(), ".egg-info") {
				return filepath.SkipDir
			}
			return nil
		}
		if !scannableExtensions[filepath.Ext(d.Name())] {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for lineNum, line := range strings.Split(string(raw), "\n") {
			for _, m := range adrReferenceRE.FindAllStringSubmatch(line, -1) {
				if !valid[m[1]] {
					t.Errorf("%s:%d: references ADR %s, which has no docs/adr file -- fix the reference (typo, or the ADR was renumbered) or add the missing record.", path, lineNum+1, m[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
}

// TestADRReferenceRegexpIgnoresNumericLiterals pins the false-positive
// case named directly in issue #79 item 4:
// internal/metrics/metrics_test.go contains the float literal 0.0012,
// which must not be mistaken for a reference to ADR 0012. adrReferenceRE
// avoids this by requiring the literal "ADR " prefix, not just four
// consecutive digits; this test fixes that behaviour so a future edit to
// the regexp (e.g. loosening it to catch a reference form that lacks the
// prefix) can't silently reintroduce the false positive without a test
// failing here first.
func TestADRReferenceRegexpIgnoresNumericLiterals(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string // expected captured ADR numbers, in order
	}{
		{
			name: "bare float literal, no ADR prefix",
			line: `r.ObserveRequest("POST /runs", 201, 0.0012)`,
			want: nil,
		},
		{
			name: "genuine reference",
			line: `// see ADR 0012 for why non-terminal runs are excluded.`,
			want: []string{"0012"},
		},
		{
			name: "genuine reference immediately followed by punctuation",
			line: `ADR 0005's "Revisited" section named this precisely.`,
			want: []string{"0005"},
		},
		{
			name: "two references on one line",
			line: `ADR 0014 (supersedes ADR 0005)`,
			want: []string{"0014", "0005"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, m := range adrReferenceRE.FindAllStringSubmatch(c.line, -1) {
				got = append(got, m[1])
			}
			if len(got) != len(c.want) {
				t.Fatalf("adrReferenceRE on %q: got %v, want %v", c.line, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("adrReferenceRE on %q: got %v, want %v", c.line, got, c.want)
					break
				}
			}
		})
	}
}
