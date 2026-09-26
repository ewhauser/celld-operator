package controller

import (
	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
)

func runtimeImage(f *fleet.CelldFleet) string { return f.Spec.RuntimeImage }

// This validates a pin, not release qualification.
func knownRuntime(image string) bool { return fleet.ValidRuntimeImage(image) }
