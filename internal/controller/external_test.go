package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestScaleStatusCountsNonTerminalPods(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	now := metav1.Now()
	pods := []client.Object{
		&corev1.Pod{Name: "a", Namespace: f.Namespace, UID: types.UID("a"), Labels: labels(f), Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{Name: "b", Namespace: f.Namespace, UID: types.UID("b"), Labels: labels(f), DeletionTimestamp: &now, Finalizers: []string{"test"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{Name: "c", Namespace: f.Namespace, UID: types.UID("c"), Labels: labels(f), Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		&corev1.Pod{Name: "other", Namespace: f.Namespace, UID: types.UID("o"), Labels: map[string]string{FleetLabel: "someone-else"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	}
	r := setup(t, append(pods, f)...)
	r.observeReplicaCounts(t.Context(), f)
	if f.Status.Replicas != 2 || f.Status.TerminatingReplicas != 1 {
		t.Fatalf("scale status wrong: replicas=%d terminating=%d", f.Status.Replicas, f.Status.TerminatingReplicas)
	}
	if f.Status.LabelSelector != FleetLabel+"="+string(f.UID) {
		t.Fatalf("label selector %q", f.Status.LabelSelector)
	}
}
