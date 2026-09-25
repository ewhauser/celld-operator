package controller

import (
	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func runtimeImage(f *fleet.CelldFleet) string { return f.Spec.RuntimeImage }

// This validates a pin, not release qualification. Strict capability and exact
// generation are independently required at the runtime/launcher boundary.
func knownRuntime(image string) bool { return fleet.ValidRuntimeImage(image) }
func runtimeNode(f *fleet.CelldFleet, p *corev1.Pod) string {
	if f.Spec.Profile == "Bucket" {
		return string(p.UID)
	}
	return p.Name
}
