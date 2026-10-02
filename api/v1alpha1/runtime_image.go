package v1alpha1

import "regexp"

// The repository pattern and fixed digest checks together match the CRD marker
// on RuntimeImage. A registry and repository path are mandatory so Kubernetes
// never substitutes an implicit registry for a supposedly pinned artifact.
var runtimeImageRepositoryPattern = regexp.MustCompile(`^([a-z0-9]+([.-][a-z0-9]+)*|localhost)(:[0-9]{1,5})?(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)+$`)

func ValidRuntimeImage(image string) bool {
	const separator = "@sha256:"
	const digestLength = 64
	repositoryEnd := len(image) - len(separator) - digestLength
	if repositoryEnd <= 0 || image[repositoryEnd:len(image)-digestLength] != separator {
		return false
	}
	for i := len(image) - digestLength; i < len(image); i++ {
		c := image[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	// Checking the fixed suffix directly avoids extending regexp's matching
	// scratch buffer and scanning it with the repository state machine.
	return runtimeImageRepositoryPattern.MatchString(image[:repositoryEnd])
}
