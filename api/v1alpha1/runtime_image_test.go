package v1alpha1

import (
	"regexp"
	"strings"
	"testing"
)

func FuzzRuntimeImageValidation(f *testing.F) {
	// The full CRD admission pattern is the oracle for the optimized validator.
	admission := regexp.MustCompile(`^([a-z0-9]+([.-][a-z0-9]+)*|localhost)(:[0-9]{1,5})?(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)+@sha256:[a-f0-9]{64}$`)
	for _, repository := range []string{
		"ghcr.io/ewhauser/celld",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld",
		"localhost:5000/team/a.b_c__d--e",
		"registry.example.com/" + strings.Repeat("segment/", 50) + "celld",
		"celld", "registry.example.com/no:tag", "UPPERCASE.example.com/celld",
	} {
		f.Add(repository + "@sha256:" + strings.Repeat("a", 64))
	}
	for _, image := range []string{
		"", "@sha256:", "ghcr.io/celld@sha256:",
		"ghcr.io/celld@sha256:" + strings.Repeat("A", 64),
		"ghcr.io/celld@sha256:" + strings.Repeat("a", 63),
		"ghcr.io/celld@sha256:" + strings.Repeat("a", 65),
		"ghcr.io/celld@sha256:" + strings.Repeat("a", 64) + "\n",
	} {
		f.Add(image)
	}
	f.Fuzz(func(t *testing.T, image string) {
		if got, want := ValidRuntimeImage(image), admission.MatchString(image); got != want {
			t.Fatalf("image validation differs from admission for %q: got %t want %t", image, got, want)
		}
	})
}
