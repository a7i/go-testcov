package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// reused regex
var inlineIgnore = "//.*untested section(\\s|,|$)"
var anyInlineIgnore = regexp.MustCompile(inlineIgnore)
var startsWithInlineIgnore = regexp.MustCompile("^\\s*" + inlineIgnore)
var perFileIgnore = regexp.MustCompile("// *untested sections: *([0-9]+)")
var generatedFile = regexp.MustCompile("/*generated.*\\.go$")

// test injection point to enable test coverage of exit behavior
var exitFunction func(code int) = os.Exit

// delegate to runGoTestAndCheckCoverage, so we have an easy to test method
func main() {
	argv := os.Args[1:len(os.Args)] // remove executable name
	exitFunction(runGoTestAndCheckCoverage(argv))
}

// run go test with given arguments + coverage and inspect coverage after run
func runGoTestAndCheckCoverage(argv []string) (exitCode int) {
	coveragePath := "coverage.out"
	_ = os.Remove(coveragePath) // remove file if it exists, to avoid confusion when test run fails

	// allow users to keep the coverage.out file when they passed -cover manually
	// TODO: parse options to find the location the user wanted and use+keep that
	if !containsString(argv, "-cover") {
		defer os.Remove(coveragePath)
	}

	argv = append([]string{"test"}, argv...)
	argv = append(argv, "-coverprofile", coveragePath)
	exitCode = runCommand("go", argv...)

	if exitCode != 0 {
		return exitCode
	}
	return checkCoverage(coveragePath)
}

// check coverage for each path that has coverage
func checkCoverage(coverageFilePath string) (exitCode int) {
	exitCode = 0
	untestedSections := untestedSections(coverageFilePath)
	sectionsByPath := groupSectionsByPath(untestedSections)

	wd, err := os.Getwd()
	check(err)

	iterateBySortedKey(sectionsByPath, func(path string, sections []Section) {
		// skip generated files since their coverage does not matter and would often have gaps
		if generatedFile.MatchString(path) {
			return
		}

		displayPath, readPath := normalizeCoveredPath(path, wd)
		configuredUntested, configuredUntestedAtLine := configuredUntestedForFile(readPath)
		lines := strings.Split(readFile(readPath), "\n")
		sections = removeSectionsMarkedWithInlineComment(sections, lines)
		actualUntested := len(sections)
		details := fmt.Sprintf("(%v current vs %v configured)", actualUntested, configuredUntested)

		if actualUntested == configuredUntested {
			// exactly as much as we expected, nothing to do
		} else if actualUntested > configuredUntested {
			printUntestedSections(sections, displayPath, details)
			exitCode = 1 // at least 1 failure, so say to add more tests
		} else {
			_, _ = fmt.Fprintf(
				os.Stderr,
				"%v has less untested sections %v, decrement configured untested?\nconfigured on: %v:%v",
				displayPath, details, readPath, configuredUntestedAtLine)
		}
	})

	return exitCode
}

func printUntestedSections(sections []Section, displayPath string, details string) {
	// TODO: color when tty
	_, _ = fmt.Fprintf(os.Stderr, "%v new untested sections introduced %v\n", displayPath, details)

	// sort sections since go coverage output is not sorted
	sort.Slice(sections, func(i, j int) bool {
		return sections[i].sortValue < sections[j].sortValue
	})

	// print copy-paste friendly snippets
	for _, section := range sections {
		_, _ = fmt.Fprintln(os.Stderr, displayPath+":"+section.Location())
	}
}

// keep untested sections that are marked with "untested section" comment
// need to be careful to not change the list while iterating, see https://pauladamsmith.com/blog/2016/07/go-modify-slice-iteration.html
// NOTE: this is a bit rough as it does not account for partial lines via start/end characters
// TODO: warn about sections that have a comment but are not uncovered
func removeSectionsMarkedWithInlineComment(sections []Section, lines []string) []Section {
	uncheckedSections := sections
	sections = []Section{}
	for _, section := range uncheckedSections {
		for lineNumber := section.startLine; lineNumber <= section.endLine; lineNumber++ {
			if anyInlineIgnore.MatchString(lines[lineNumber-1]) {
				break // section is ignored
			} else if lineNumber >= 2 && startsWithInlineIgnore.MatchString(lines[lineNumber-2]) {
				break // section is ignored by inline ignore above it
			} else if lineNumber == section.endLine {
				sections = append(sections, section) // keep the section
			}
		}
	}
	return sections
}

func groupSectionsByPath(sections []Section) (grouped map[string][]Section) {
	grouped = map[string][]Section{}
	for _, section := range sections {
		path := section.path
		group, ok := grouped[path]
		if !ok {
			grouped[path] = []Section{}
		}
		grouped[path] = append(group, section)
	}
	return
}

// Find the untested sections given a coverage path
func untestedSections(coverageFilePath string) (sections []Section) {
	sections = []Section{}
	content := readFile(coverageFilePath)

	lines := splitWithoutEmpty(content, '\n')

	// remove the initial `set: mode` line
	if len(lines) == 0 {
		return
	}
	lines = lines[1:]

	// we want lines that end in " 0", they have no coverage
	for _, line := range lines {
		if strings.HasSuffix(line, " 0") {
			sections = append(sections, NewSection(line))
		}
	}

	return
}

// find relative path of file in current directory
func findFile(path string) (readPath string) {
	parts := strings.Split(path, string(os.PathSeparator))
	for len(parts) > 0 {
		_, err := os.Stat(strings.Join(parts, string(os.PathSeparator)))
		if err != nil {
			parts = parts[1:] // shift directory to continue to look for file
		} else {
			break
		}
	}
	return strings.Join(parts, string(os.PathSeparator))
}

// remove path prefix like "github.com/user/lib", but cache the call to os.Get
func normalizeCoveredPath(path string, workingDirectory string) (displayPath string, readPath string) {
	modulePrefixSize := 3 // foo.com/bar/baz + file.go
	separator := string(os.PathSeparator)
	parts := strings.SplitN(path, separator, modulePrefixSize+1)
	goPath, hasGoPath := os.LookupEnv("GOPATH")
	inGoPath := false
	goPrefixedPath := joinPath(goPath, "src", path)

	if hasGoPath {
		_, err := os.Stat(goPrefixedPath)
		inGoPath = !os.IsNotExist(err)
	}

	// path too short, return a good guess
	if len(parts) <= modulePrefixSize {
		if inGoPath {
			return path, goPrefixedPath
		} else {
			return path, path
		}
	}

	prefix := strings.Join(parts[:modulePrefixSize], separator)
	demodularized := findFile(strings.SplitN(path, prefix+separator, 2)[1])

	// folder is not in go path ... remove module nesting
	if !inGoPath {
		return demodularized, demodularized
	}

	// we are in a nested folder ... remove module nesting and expand full goPath
	if strings.HasSuffix(workingDirectory, prefix) {
		return demodularized, goPrefixedPath
	}

	// testing remote package, don't expand display but expand full goPath
	return path, goPrefixedPath
}

// How many sections are expected to be untested, 0 if not configured
// also return at what line we found the comment so we can point the user to it
func configuredUntestedForFile(path string) (count int, lineNumber int) {
	content := readFile(path)
	match := perFileIgnore.FindStringSubmatch(content)
	if len(match) == 2 {
		index := perFileIgnore.FindStringIndex(content)[0]
		linesBeforeMatch := strings.Count(content[0:index], "\n")
		return stringToInt(match[1]), linesBeforeMatch + 1
	} else {
		return 0, 0
	}
}
