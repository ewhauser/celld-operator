package controller

import (
	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/catalog"
)

func runtimeImage(f *fleet.CelldFleet) string {
	if f.Spec.RuntimeImage != "" {
		return f.Spec.RuntimeImage
	}
	return Image
}
func knownRuntime(image string) bool { _, ok := catalog.Lookup(image); return ok }
func appliedRuntime(f *fleet.CelldFleet, j *lifecycleJournal) *fleet.CelldFleet {
	current := f.DeepCopy()
	current.Spec.RuntimeImage = j.RuntimeImage
	return current
}

// Runtime observations follow durable applied authority even when the user has
// already requested another image. Only the all-stopped resume phase selects
// the installed target before completion promotes RuntimeImage.
func evidenceRuntime(f *fleet.CelldFleet, j *lifecycleJournal) *fleet.CelldFleet {
	image := j.RuntimeImage
	if m := j.Maintenance; m != nil && m.Kind == "Upgrade" && m.Phase == "Resuming" {
		image = m.TargetImage
	}
	if runtimeImage(f) == image {
		return f
	}
	current := f.DeepCopy()
	current.Spec.RuntimeImage = image
	return current
}
