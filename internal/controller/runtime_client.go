package controller

import (
	"errors"
	"net"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
)

// runtimeTarget addresses a fleet Pod's internal listener for both profiles.
// It requires a current, owned Pod with an exact IP; callers recheck the Pod
// incarnation after their reads.
func runtimeTarget(f *fleet.CelldFleet, p *corev1.Pod) (controlplane.Target, error) {
	identity, _ := podIdentity(p)
	if (f.Spec.Profile != "Bucket" && f.Spec.Profile != "PersistentFleet") || identity == "" || p.Namespace != f.Namespace || p.Labels[FleetLabel] != string(f.UID) || net.ParseIP(p.Status.PodIP) == nil {
		return controlplane.Target{}, errors.New("runtime target is not a current fleet pod")
	}
	return controlplane.Target{IP: p.Status.PodIP}, nil
}
