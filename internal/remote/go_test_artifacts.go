package remote

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type goTestFailureArtifact struct {
	Packages []string          `json:"packages"`
	Tests    []string          `json:"tests"`
	Files    []goTestFileFrame `json:"files"`
}

type goTestFileFrame struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

var (
	reGoTestFailPkg = regexp.MustCompile(`^FAIL\s+([^\s]+)\s+`)
	reGoTestFailOne = regexp.MustCompile(`^--- FAIL:\s+([^\s]+)\s+\(`)
	reGoTestFileLoc = regexp.MustCompile(`^(\S+\.go):(\d+):\s?(.*)$`)
)

func extractGoTestFailureArtifact(command string, output string) (title string, payloadJSON []byte, ok bool) {
	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, "go test") {
		return "", nil, false
	}

	var pkgs []string
	pkgSet := map[string]struct{}{}
	var tests []string
	testSet := map[string]struct{}{}
	var files []goTestFileFrame
	fileSeen := map[string]struct{}{}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := reGoTestFailPkg.FindStringSubmatch(line); len(m) == 2 {
			if _, exists := pkgSet[m[1]]; !exists {
				pkgSet[m[1]] = struct{}{}
				pkgs = append(pkgs, m[1])
			}
			continue
		}
		if m := reGoTestFailOne.FindStringSubmatch(line); len(m) == 2 {
			if _, exists := testSet[m[1]]; !exists {
				testSet[m[1]] = struct{}{}
				tests = append(tests, m[1])
			}
			continue
		}
		if m := reGoTestFileLoc.FindStringSubmatch(line); len(m) == 4 {
			key := m[1] + ":" + m[2]
			if _, exists := fileSeen[key]; exists {
				continue
			}
			fileSeen[key] = struct{}{}
			files = append(files, goTestFileFrame{
				File: m[1],
				Line: atoiBestEffort(m[2]),
				Text: m[3],
			})
			// Keep artifacts small.
			if len(files) >= 25 {
				continue
			}
		}
	}

	if len(pkgs) == 0 && len(tests) == 0 {
		return "", nil, false
	}
	sort.Strings(pkgs)
	sort.Strings(tests)

	title = "go test failures"
	if len(tests) == 1 {
		title = "go test FAIL: " + tests[0]
	} else if len(pkgs) == 1 && len(tests) == 0 {
		title = "go test FAIL: " + pkgs[0]
	}

	body := goTestFailureArtifact{
		Packages: pkgs,
		Tests:    tests,
		Files:    files,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", nil, false
	}
	return title, b, true
}

func atoiBestEffort(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
