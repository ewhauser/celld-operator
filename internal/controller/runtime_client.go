package controller

import (
	"errors"
	"net"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
)

// runtimeTarget is the common addressing seam for both profiles. A lifecycle
// caller supplies its exact recorded generation; the collector leaves it empty
// to discover the current runtime identity from the new schema. The caller
// must still recheck the Pod incarnation after observations and retain fences.
func runtimeTarget(f *fleet.CelldFleet, p *corev1.Pod, generation string) (controlplane.Target, error) {
	identity, _ := podIdentity(p)
	if (f.Spec.Profile != "Bucket" && f.Spec.Profile != "PersistentFleet") || identity == "" || p.Namespace != f.Namespace || p.Labels[FleetLabel] != string(f.UID) || net.ParseIP(p.Status.PodIP) == nil {
		return controlplane.Target{}, errors.New("runtime target is not a current fleet pod")
	}
	return controlplane.Target{IP: p.Status.PodIP, Node: runtimeNode(f, p), Generation: generation}, nil
}
