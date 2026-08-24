package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/frostyard/snowcat-cockpit/internal/queueview"
)

const (
	beginMarker = "<!-- BEGIN GENERATED ROLE/KIND CLASSIFICATION -->"
	endMarker   = "<!-- END GENERATED ROLE/KIND CLASSIFICATION -->"
)

func main() {
	path := flag.String("path", "", "worker-profiles specification path")
	flag.Parse()
	if *path == "" {
		fail(errors.New("worker-profiles specification path is required"))
	}
	content, err := os.ReadFile(*path)
	if err != nil {
		fail(fmt.Errorf("read worker-profiles specification: %w", err))
	}
	updated, err := replaceGeneratedTable(string(content))
	if err != nil {
		fail(err)
	}
	if updated == string(content) {
		return
	}
	if err := os.WriteFile(*path, []byte(updated), 0o644); err != nil {
		fail(fmt.Errorf("write worker-profiles specification: %w", err))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func replaceGeneratedTable(document string) (string, error) {
	start := strings.Index(document, beginMarker)
	end := strings.Index(document, endMarker)
	if start < 0 || end < 0 || end < start {
		return "", errors.New("worker-profiles specification lacks classifier generation markers")
	}
	if strings.Count(document, beginMarker) != 1 || strings.Count(document, endMarker) != 1 {
		return "", errors.New("worker-profiles specification has duplicate classifier generation markers")
	}
	end += len(endMarker)
	return document[:start] + generatedTable() + document[end:], nil
}

func generatedTable() string {
	var output strings.Builder
	output.WriteString(beginMarker)
	output.WriteString("\n\n| Ordered kind match | Role |\n| --- | --- |\n")
	for _, rule := range queueview.ClassificationRules() {
		fmt.Fprintf(&output, "| %s | `%s` |\n", renderMatch(rule), rule.Role)
	}
	output.WriteString("\n")
	output.WriteString(endMarker)
	return output.String()
}

func renderMatch(rule queueview.ClassificationRule) string {
	switch rule.Match {
	case queueview.MatchSuffix:
		return "suffix `" + rule.Suffix + "`"
	case queueview.MatchExact:
		values := make([]string, len(rule.ExactKinds))
		for index, kind := range rule.ExactKinds {
			values[index] = "`" + kind + "`"
		}
		if len(values) == 1 {
			return "exact " + values[0]
		}
		return "exact " + strings.Join(values[:len(values)-1], ", ") + ", or " + values[len(values)-1]
	case queueview.MatchFallback:
		return "every other kind"
	default:
		return "unknown classifier rule"
	}
}
