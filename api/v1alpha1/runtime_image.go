package v1alpha1

import "regexp"

// runtimeImagePattern is kept in step with the CRD validation marker on RuntimeImage.
// A registry and repository path are mandatory so Kubernetes never substitutes
// an implicit registry for a supposedly pinned artifact.
var runtimeImagePattern = regexp.MustCompile(`^([a-z0-9]+([.-][a-z0-9]+)*|localhost)(:[0-9]{1,5})?(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)+@sha256:[a-f0-9]{64}$`)

func ValidRuntimeImage(image string) bool { return runtimeImagePattern.MatchString(image) }
