package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestPersistentPeerAddressSurvivesPodIPReplacement(t *testing.T) {
	f := fixture("durable", "data", "PersistentFleet")
	f.Namespace = "other-namespace"
	w := workload(f, Options{}).(*appsv1.StatefulSet)
	env := w.Spec.Template.Spec.Containers[0].Env
	want := "$(CELLD_NODE)." + w.Spec.ServiceName + ".other-namespace.svc:8081"
	if got, _ := envValue(env, "CELLD_ADVERTISE"); got != want {
		t.Fatalf("persistent predecessor recovery needs a stable peer address: got %q, want %q", got, want)
	}
	// Kubernetes expands only preceding variables. The address must refer to
	// the stable ordinal name, not a replacement Pod UID or its new IP.
	foundNode := false
	for _, variable := range env {
		if variable.Name == "CELLD_NODE" {
			foundNode = variable.ValueFrom != nil && variable.ValueFrom.FieldRef != nil && variable.ValueFrom.FieldRef.FieldPath == "metadata.name"
		}
		if variable.Name == "CELLD_ADVERTISE" && !foundNode {
			t.Fatal("advertised address cannot expand the preceding stable node name")
		}
	}
	// Recovery serves follower traffic before readiness. Withholding DNS until
	// readiness would leave both retained disks unable to find their witness.
	foundPeers := false
	for _, object := range prerequisites(f, Options{}) {
		service, ok := object.(*corev1.Service)
		if ok && service.Name == w.Spec.ServiceName {
			foundPeers = true
			if service.Namespace != f.Namespace || service.Spec.ClusterIP != corev1.ClusterIPNone || !service.Spec.PublishNotReadyAddresses {
				t.Fatal("persistent peer DNS is unavailable during predecessor recovery")
			}
		}
	}
	if !foundPeers {
		t.Fatal("StatefulSet has no peer DNS service")
	}
}

func TestBucketPeerAddressUsesItsNewPodIncarnation(t *testing.T) {
	for _, layout := range []string{"Deployment", "Ordered"} {
		f := fixture("bucket", "data", "Bucket")
		f.Spec.BucketWorkload = layout
		env := podTemplate(f, Options{}).Spec.Containers[0].Env
		if got, _ := envValue(env, "CELLD_ADVERTISE"); got != "$(POD_IP):8081" {
			t.Fatalf("%s Bucket address = %q", layout, got)
		}
	}
}
