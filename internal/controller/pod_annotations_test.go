package controller

import (
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const skipContainers = "eks.amazonaws.com/skip-containers"

// Pod annotations sit beside the operator's own, which they cannot replace.
func TestPodAnnotationsRenderBesideOperatorAnnotations(t *testing.T) {
	f := meshFixture("PersistentFleet")
	f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
	f.Spec.PodAnnotations = map[string]string{skipContainers: "istio-proxy,istio-validation"}
	template := podTemplate(f, Options{})
	if template.Annotations[skipContainers] != "istio-proxy,istio-validation" {
		t.Fatalf("pod annotation not rendered: %v", template.Annotations)
	}
	if template.Annotations[restartTokenAnnotation] != "r1" || template.Annotations[istioNativeSidecar] != "true" || template.Annotations[istioExcludeOutbound] != podIdentityAgentCIDR {
		t.Fatalf("operator annotations lost: %v", template.Annotations)
	}
	template.Annotations["example.com/x"] = "y"
	if _, ok := f.Spec.PodAnnotations["example.com/x"]; ok {
		t.Fatal("template shares the fleet's map")
	}
}

// Pod annotations are not part of the reservation, so adding them to a live
// fleet rolls the template instead of conflicting with the frozen hash.
func TestPodAnnotationsChangeRollsOut(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	r := setup(t, f)
	f = reconcile(t, r, f)
	f.Spec.PodAnnotations = map[string]string{skipContainers: "istio-proxy"}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	got := reconcile(t, r, f)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Ready"); c != nil && c.Reason == "StorageScopeConflict" {
		t.Fatalf("pod annotation change refused: %s", c.Message)
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Annotations[skipContainers] != "istio-proxy" {
		t.Fatalf("pod annotation not rolled out: %v", sts.Spec.Template.Annotations)
	}
	if !matches(workload(got, r.Options), sts) {
		t.Fatal("workload does not match the changed fleet")
	}
}
