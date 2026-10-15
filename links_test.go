package reconcilerfuzzer_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/report"
)

// A page that moved takes its headings with it, so each link from a page a
// reader follows must still land.
func TestThePagesLinksLand(t *testing.T) {
	link := regexp.MustCompile(`\]\(([^)#\s]*)(?:#([^)\s]+))?\)`)
	checked := 0
	spikes, err := filepath.Glob("docs/spikes/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range append(append(slices.Clone(userPages), "docs/reference.md"), spikes...) {
		for _, m := range link.FindAllStringSubmatch(readFile(t, page), -1) {
			target, anchor := m[1], m[2]
			if strings.Contains(target, "://") {
				continue
			}
			if target == "" {
				target = page
			} else {
				target = path.Join(path.Dir(page), target)
			}
			checked++
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s links %q: %v", page, m[0], err)
				continue
			}
			if anchor != "" && !slices.Contains(anchors(readFile(t, target)), anchor) {
				t.Errorf("%s links %q, and %s has no heading with that anchor.", page, m[0], target)
			}
		}
	}
	if checked == 0 {
		t.Fatal("The pages hold no link, so this test checks nothing.")
	}
}

var heading = regexp.MustCompile(`^#+ (.*)$`)

// anchors are the fragments GitHub gives the headings of a Markdown page
// outside its fenced blocks.
func anchors(page string) []string {
	var found []string
	fenced := false
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		title := heading.FindStringSubmatch(line)
		if fenced || title == nil {
			continue
		}
		found = append(found, strings.Map(func(r rune) rune {
			switch {
			case r == ' ':
				return '-'
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
				return unicode.ToLower(r)
			}
			return -1
		}, title[1]))
	}
	return found
}

func TestAnchorsFollowGitHub(t *testing.T) {
	page := "# reconciler-fuzzer\n\n## Keep reconciler-fuzzer out of your go.mod\n\n```sh\n# not a heading\n```\n\n### 6. Generic invariants\n## `target.yaml` keys\n"
	if got, want := anchors(page), []string{"reconciler-fuzzer", "keep-reconciler-fuzzer-out-of-your-gomod", "6-generic-invariants", "targetyaml-keys"}; !slices.Equal(got, want) {
		t.Errorf("anchors gave %q, want %q.", got, want)
	}
}

// A report links the section of the checks page that its check has.
func TestReportsLinkTheirChecksSection(t *testing.T) {
	page := anchors(readFile(t, checksPage))
	if !slices.Contains(page, "properties") {
		t.Errorf("%s has no Properties section for a property's report to link.", checksPage)
	}
	sections := map[string]string{"P1": "properties"}
	for _, anchor := range page {
		if id, _, found := strings.Cut(anchor, "-"); found && checkName.MatchString(strings.ToUpper(id)) {
			sections[strings.ToUpper(id)] = anchor
		}
	}
	if len(sections) == 1 {
		t.Fatalf("%s has no section for a generic check, so this test checks only properties.", checksPage)
	}
	for id, anchor := range sections {
		if got, want := report.CheckSection(id), report.ChecksPage+"#"+anchor; got != want {
			t.Errorf("A %s report links %q, want %q.", id, got, want)
		}
	}
}
