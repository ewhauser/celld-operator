package controller

import (
	"errors"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

func validatePersistentPod(f *fleet.CelldFleet, pod *corev1.Pod, opts Options) error {
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.InitContainers) != 1 || pod.Spec.HostPID || pod.Spec.HostNetwork || pod.Spec.HostIPC || !pod.DeletionTimestamp.IsZero() {
		return errors.New("unsupported persistent pod composition")
	}
	expected := podTemplate(f, opts).Spec
	actual := *pod.Spec.DeepCopy()
	normalizePod(&expected)
	normalizePod(&actual)
	got, want := actual.Containers[0], expected.Containers[0]
	if !equality.Semantic.DeepEqual(got.SecurityContext, want.SecurityContext) || !equality.Semantic.DeepEqual(actual.SecurityContext, expected.SecurityContext) || !equality.Semantic.DeepEqual(actual.AutomountServiceAccountToken, expected.AutomountServiceAccountToken) || got.Image != runtimeImage(f) || !slices.Equal(got.Command, want.Command) || len(got.Args) != 0 || len(got.EnvFrom) != 0 || got.WorkingDir != "" || actual.ServiceAccountName != expected.ServiceAccountName {
		return errors.New("persistent runtime invocation differs")
	}
	init := actual.InitContainers[0]
	wantInit := expected.InitContainers[0]
	if !equality.Semantic.DeepEqual(init.SecurityContext, wantInit.SecurityContext) || init.Name != wantInit.Name || init.Image != wantInit.Image || !slices.Equal(init.Command, wantInit.Command) || len(init.Args) != 0 || len(init.Env) != 0 || len(init.EnvFrom) != 0 || !equality.Semantic.DeepEqual(init.VolumeMounts, wantInit.VolumeMounts) {
		return errors.New("launcher installer differs")
	}
	seen := map[string]bool{}
	for _, env := range got.Env {
		if seen[env.Name] {
			return errors.New("duplicate runtime environment")
		}
		seen[env.Name] = true
		index := slices.IndexFunc(want.Env, func(e corev1.EnvVar) bool { return e.Name == env.Name })
		if index >= 0 {
			if !equality.Semantic.DeepEqual(env, want.Env[index]) {
				return errors.New("runtime environment differs")
			}
		} else {
			switch env.Name {
			case "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_STS_REGIONAL_ENDPOINTS", "AWS_DEFAULT_REGION":
			default:
				return errors.New("unsupported runtime environment")
			}
		}
	}
	for _, env := range want.Env {
		if !seen[env.Name] {
			return errors.New("runtime environment missing")
		}
	}
	for _, mount := range want.VolumeMounts {
		if !slices.ContainsFunc(got.VolumeMounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, mount) }) {
			return errors.New("runtime mount differs")
		}
	}
	for _, mount := range got.VolumeMounts {
		if slices.ContainsFunc(want.VolumeMounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, mount) }) {
			continue
		}
		if !mount.ReadOnly || (mount.MountPath != "/var/run/secrets/eks.amazonaws.com/serviceaccount" && mount.MountPath != "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount") {
			return errors.New("unsupported runtime mount")
		}
	}
	for _, volume := range expected.Volumes {
		if !slices.ContainsFunc(actual.Volumes, func(v corev1.Volume) bool { return equality.Semantic.DeepEqual(v, volume) }) {
			return errors.New("launcher volume differs")
		}
	}
	if f.Spec.Profile == "PersistentFleet" {
		if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
			return v.Name == "data" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "data-"+pod.Name && !v.PersistentVolumeClaim.ReadOnly
		}) {
			return errors.New("pod data volume does not match the exact ordinal claim")
		}
	}
	return nil
}
