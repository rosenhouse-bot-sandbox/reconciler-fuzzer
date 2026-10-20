package reconcilerfuzzer_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestReadmeCommandsAndPathsExist(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range readmeProblems(t, string(readme)) {
		t.Error(problem)
	}
}

func TestReadmeProblemsCatchesMistakes(t *testing.T) {
	const good = "See [failures](docs/failures.md) and targets/toy-widget/config.yaml.\n\n" +
		"```sh\nmake setup\nexport KUBEBUILDER_ASSETS=$(make -s assets-path)\n" +
		"reconciler-fuzzer run --config c.yaml --plan p.json \\\n  --launch-arg --bug=3 --out o\n" +
		"reconciler-fuzzer hunt --config c.yaml --seeds 5 --out o\n```\n"
	if problems := readmeProblems(t, good); len(problems) > 0 {
		t.Fatalf("a good README gave %q", problems)
	}
	for name, mutate := range map[string][2]string{
		"unknown make target":     {"make setup", "make no-such-target"},
		"unknown target in $()":   {"make -s assets-path", "make -s no-such-path"},
		"unknown command":         {"reconciler-fuzzer hunt", "reconciler-fuzzer fuzz"},
		"flag of another command": {"--seeds 5", "--plan 5"},
		"continued line flag":     {"--launch-arg --bug=3", "--launch-args --bug=3"},
		"missing path in prose":   {"targets/toy-widget/config.yaml", "targets/toy-widget/nope.yaml"},
		"missing link target":     {"(docs/failures.md)", "(docs/nope.md)"},
	} {
		mutated := strings.Replace(good, mutate[0], mutate[1], 1)
		if mutated == good {
			t.Fatalf("%s: %q is not in the good README", name, mutate[0])
		}
		if problems := readmeProblems(t, mutated); len(problems) == 0 {
			t.Errorf("%s: no problem found", name)
		}
	}
	if problems := readmeProblems(t, "no code"); len(problems) == 0 {
		t.Error("a README with no commands gave no problem")
	}
}

var (
	shellBlock    = regexp.MustCompile("(?s)```(?:sh|bash|shell)\n(.*?)```")
	makeCall      = regexp.MustCompile(`\bmake((?:\s+-\S+)*)\s+([a-z][\w-]*)`)
	makefileRule  = regexp.MustCompile(`(?m)^([a-z][\w-]*):`)
	usageLine     = regexp.MustCompile(`(?m)^\s*reconciler-fuzzer (\w+)\s+(.*)$`)
	longFlag      = regexp.MustCompile(`--[a-z][\w-]*`)
	repoPath      = regexp.MustCompile(`\b(?:targets|examples|docs)/[\w./-]*[\w/]`)
	relativeLink  = regexp.MustCompile(`\]\(([^)#:\s]+)[^)]*\)`)
	continuedLine = regexp.MustCompile(`\\\n\s*`)
)

// readmeProblems lists each make target, reconciler-fuzzer command, flag and
// repo path in readme that does not exist.
func readmeProblems(t *testing.T, readme string) []string {
	targets := makeTargets(t)
	commands := commandFlags(t)
	var problems []string
	makes, runs := 0, 0
	for _, block := range shellBlock.FindAllStringSubmatch(readme, -1) {
		for line := range strings.SplitSeq(continuedLine.ReplaceAllString(block[1], " "), "\n") {
			for _, call := range makeCall.FindAllStringSubmatch(line, -1) {
				makes++
				if !slices.Contains(targets, call[2]) {
					problems = append(problems, "the Makefile has no target "+call[2])
				}
			}
			fields := strings.Fields(line)
			for i, field := range fields {
				if field != "reconciler-fuzzer" || i+1 == len(fields) {
					continue
				}
				runs++
				flags, ok := commands[fields[i+1]]
				if !ok {
					problems = append(problems, "reconciler-fuzzer has no command "+fields[i+1])
					continue
				}
				args := fields[i+2:]
				for j := 0; j < len(args); j++ {
					flag, _, _ := strings.Cut(args[j], "=")
					if strings.HasPrefix(flag, "--") && !slices.Contains(flags, flag) {
						problems = append(problems, "reconciler-fuzzer "+fields[i+1]+" has no flag "+flag)
					}
					if flag == "--launch-arg" {
						j++ // Its value goes to the controller.
					}
				}
			}
		}
	}
	if makes == 0 || runs == 0 {
		problems = append(problems, "the README's shell blocks show no make target or no reconciler-fuzzer command")
	}
	paths := repoPath.FindAllString(readme, -1)
	for _, link := range relativeLink.FindAllStringSubmatch(readme, -1) {
		paths = append(paths, link[1])
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			problems = append(problems, "the README names "+path+", which does not exist")
		}
	}
	return problems
}

func makeTargets(t *testing.T) []string {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, rule := range makefileRule.FindAllStringSubmatch(string(makefile), -1) {
		targets = append(targets, rule[1])
	}
	return targets
}

// commandFlags reads each command's flags from the usage string in main.go.
func commandFlags(t *testing.T) map[string][]string {
	main, err := os.ReadFile("cmd/reconciler-fuzzer/main.go")
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string][]string{}
	for _, line := range usageLine.FindAllStringSubmatch(string(main), -1) {
		commands[line[1]] = longFlag.FindAllString(line[2], -1)
	}
	if len(commands) == 0 {
		t.Fatal("main.go has no usage lines")
	}
	return commands
}
