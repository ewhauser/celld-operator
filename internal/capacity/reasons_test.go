package capacity

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Reasons matches the reason literals the policy and its controller can write
// to status.capacity.reason, in both directions.
func TestReasonsListEveryEmittedReason(t *testing.T) {
	emitted := regexp.MustCompile(`(?:hold\(|Reason: |return )"([A-Z][A-Za-z]+)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join("..", "controller", "capacity.go"))
	seen := map[string]bool{}
	for _, file := range files {
		if file == "reasons.go" || strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range emitted.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
			if !slices.Contains(Reasons, m[1]) {
				t.Errorf("%s emits reason %q missing from Reasons", file, m[1])
			}
		}
	}
	for _, reason := range Reasons {
		if !seen[reason] {
			t.Errorf("Reasons lists %q, which nothing emits", reason)
		}
	}
}
