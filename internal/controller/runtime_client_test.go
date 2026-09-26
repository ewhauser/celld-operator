package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeTargetRequiresCurrentFleetPod(t *testing.T) {
	for _, profile := range []string{"PersistentFleet", "Bucket"} {
		t.Run(profile, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", profile)
			pod := &corev1.Pod{Name: "alpha-0", Namespace: f.Namespace, UID: "pod-uid", Labels: labels(f), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "celld", Image: fixtureRuntime}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
			if target, err := runtimeTarget(f, pod); err != nil || target.IP != pod.Status.PodIP {
				t.Fatal(target, err)
			}
			for _, change := range []string{"foreign fleet", "foreign namespace", "unknown image", "deleted", "not running", "no UID", "service name"} {
				t.Run(change, func(t *testing.T) {
					bad := pod.DeepCopy()
					switch change {
					case "foreign fleet":
						bad.Labels[FleetLabel] = "other"
					case "foreign namespace":
						bad.Namespace = "other"
					case "unknown image":
						bad.Spec.Containers[0].Image = "unknown"
					case "deleted":
						now := metav1.Now()
						bad.DeletionTimestamp = &now
					case "not running":
						bad.Status.ContainerStatuses = nil
					case "no UID":
						bad.UID = ""
					case "service name":
						bad.Status.PodIP = "alpha.fleets.svc"
					}
					if _, err := runtimeTarget(f, bad); err == nil {
						t.Fatal("unsafe target accepted")
					}
				})
			}
		})
	}
}
