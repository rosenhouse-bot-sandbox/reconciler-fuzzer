package reconcilerfuzzer_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const skillDir = "skills/adopt-reconciler-fuzzer"

type skillFrontmatter struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func readSkill(t *testing.T) (skillFrontmatter, string) {
	t.Helper()
	front, body, found := strings.Cut(strings.TrimPrefix(readFile(t, skillDir+"/SKILL.md"), "---\n"), "\n---\n")
	if !found {
		t.Fatalf("%s/SKILL.md opens with no frontmatter", skillDir)
	}
	var fm skillFrontmatter
	if err := yaml.UnmarshalStrict([]byte(front), &fm); err != nil {
		t.Fatalf("%s/SKILL.md frontmatter: %v", skillDir, err)
	}
	return fm, body
}

// Claude Code lists a skill only within these limits.
func TestTheAdoptionSkillKeepsToTheSkillFormat(t *testing.T) {
	fm, body := readSkill(t)
	if fm.Name != filepath.Base(skillDir) {
		t.Errorf("The skill's name is %q, and its directory is %s.", fm.Name, skillDir)
	}
	if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(fm.Name) || strings.Contains(fm.Name, "claude") || strings.Contains(fm.Name, "anthropic") {
		t.Errorf("The skill's name %q is not up to 64 lowercase letters, digits and hyphens free of reserved words.", fm.Name)
	}
	if n := len(fm.Description); n == 0 || n > 1024 || strings.ContainsAny(fm.Description, "<>") {
		t.Errorf("The skill's description is %d characters, and must be 1 to 1024 with no tags.", n)
	}
	if n := strings.Count(body, "\n"); n >= 500 {
		t.Errorf("The skill's body is %d lines, and Claude reads it best under 500.", n)
	}
}

// moduleLink is a page the skill reads from the module the user installed.
var moduleLink = regexp.MustCompile("<module>/([A-Za-z0-9_./-]*[A-Za-z0-9_])(?:#([a-z0-9-]+))?")

// The skill reads the docs of the version the user installed, so each page and
// heading it cites must ship in this module.
func TestTheAdoptionSkillsPagesLand(t *testing.T) {
	_, body := readSkill(t)
	links := moduleLink.FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		t.Fatal("The skill cites no page, so this test checks nothing.")
	}
	for _, m := range links {
		page, anchor := m[1], m[2]
		if _, err := os.Stat(page); err != nil {
			t.Errorf("The skill cites %s: %v", m[0], err)
			continue
		}
		if anchor != "" && !slices.Contains(anchors(readFile(t, page)), anchor) {
			t.Errorf("The skill cites %s, and %s has no heading with that anchor.", m[0], page)
		}
	}
}

var installsReconcilerFuzzer = regexp.MustCompile(`go install github\.com/rosenhouse-bot-sandbox/reconciler-fuzzer/\S+`)

func TestTheAdoptionSkillInstallsWhatTheReadmeInstalls(t *testing.T) {
	_, body := readSkill(t)
	want := installsReconcilerFuzzer.FindAllString(section(t, readFile(t, "README.md"), "## Install"), -1)
	got := installsReconcilerFuzzer.FindAllString(body, -1)
	if len(got) == 0 || len(want) == 0 {
		t.Fatalf("The skill installs %q, and the README %q.", got, want)
	}
	for _, command := range got {
		if !slices.Contains(want, command) {
			t.Errorf("The skill runs %q, and the README's Install section runs %q.", command, want)
		}
	}
}

// The README's copy command copies the skill from the module the reader installed.
func TestTheReadmeCopiesTheAdoptionSkill(t *testing.T) {
	copies := regexp.MustCompile(`cp -r "\$\(go env GOMODCACHE\)/github\.com/rosenhouse-bot-sandbox/reconciler-fuzzer@\$\(reconciler-fuzzer version[^)]*\)/(\S+)" \.claude/skills/`)
	m := copies.FindStringSubmatch(readFile(t, "README.md"))
	if m == nil {
		t.Fatal("README.md has no command that copies the skill from the module cache.")
	}
	if m[1] != skillDir {
		t.Errorf("README.md copies %s, and the skill is %s.", m[1], skillDir)
	}
}
