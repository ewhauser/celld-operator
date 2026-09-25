package controller

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// validatePersistentPod checks an admitted Pod only for what would make the
// launcher's shutdown proof wrong. That proof rests on the authenticated
// launcher handshake and exact Pod, host, boot and disk identities, and the
// workload template is verified exactly elsewhere; admission may still add
// containers, environment, volumes and mounts (service meshes, credential and
// telemetry injectors) and none of that is enumerated here. Rejected: another
// container reaching the data disk, launcher binary or launcher key; shared
// host or pod process namespaces; and a changed invocation of the operator's
// own containers, which are located by name. Admission that can add privileged
// or hostPath containers to the namespace is trusted with the node and is not
// detected. Every error names the offending field or container.
func validatePersistentPod(f *fleet.CelldFleet, pod *corev1.Pod, opts Options) error {
	switch {
	case !pod.DeletionTimestamp.IsZero():
		return errors.New("pod is terminating")
	case pod.Spec.HostPID, pod.Spec.HostIPC, pod.Spec.HostNetwork:
		return errors.New("pod shares a host namespace (spec.hostPID, hostIPC or hostNetwork)")
	case pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace:
		return errors.New("pod sets spec.shareProcessNamespace; other containers could signal or hold the runtime")
	case len(pod.Spec.EphemeralContainers) != 0:
		return fmt.Errorf("pod has ephemeral container %q; debug containers can reach the runtime process", pod.Spec.EphemeralContainers[0].Name)
	}
	expected := podTemplate(f, opts).Spec
	actual := *pod.Spec.DeepCopy()
	normalizePod(&expected)
	normalizePod(&actual)
	owned := map[string]bool{}
	for _, v := range expected.Volumes {
		owned[v.Name] = true
		i := slices.IndexFunc(actual.Volumes, func(a corev1.Volume) bool { return a.Name == v.Name })
		if i < 0 || !equality.Semantic.DeepEqual(actual.Volumes[i], v) {
			return fmt.Errorf("operator volume %q differs", v.Name)
		}
	}
	if f.Spec.Profile == "PersistentFleet" {
		owned["data"] = true
		if !slices.ContainsFunc(actual.Volumes, func(v corev1.Volume) bool {
			return v.Name == "data" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "data-"+pod.Name && !v.PersistentVolumeClaim.ReadOnly
		}) {
			return errors.New("pod data volume does not match the exact ordinal claim")
		}
	}
	for _, lists := range [][2][]corev1.Container{{expected.Containers, actual.Containers}, {expected.InitContainers, actual.InitContainers}} {
		want, got := lists[0], lists[1]
		for _, w := range want {
			if !slices.ContainsFunc(got, func(c corev1.Container) bool { return c.Name == w.Name }) {
				return fmt.Errorf("operator container %q is missing", w.Name)
			}
		}
		for i := range got {
			c := &got[i]
			j := slices.IndexFunc(want, func(w corev1.Container) bool { return w.Name == c.Name })
			var template []corev1.VolumeMount
			if j >= 0 {
				if err := validateOwnedContainer(c, &want[j], f, opts); err != nil {
					return err
				}
				template = want[j].VolumeMounts
			}
			// Only the template's exact mounts may reach operator volumes; any other
			// writer or reader of the disk or launcher key invalidates the proof.
			for _, m := range c.VolumeMounts {
				if owned[m.Name] && !slices.ContainsFunc(template, func(w corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(w, m) }) {
					return fmt.Errorf("container %q mounts operator volume %q", c.Name, m.Name)
				}
			}
			for _, d := range c.VolumeDevices {
				if owned[d.Name] {
					return fmt.Errorf("container %q attaches operator volume %q as a device", c.Name, d.Name)
				}
			}
		}
	}
	return nil
}

func validateOwnedContainer(got, want *corev1.Container, f *fleet.CelldFleet, opts Options) error {
	image := opts.LauncherImage
	if want.Name == "celld" {
		image = runtimeImage(f)
	}
	if !sameArtifact(got.Image, image) {
		return fmt.Errorf("container %q image %s differs from %s", got.Name, got.Image, image)
	}
	if !slices.Equal(got.Command, want.Command) || !slices.Equal(got.Args, want.Args) || got.WorkingDir != want.WorkingDir {
		return fmt.Errorf("container %q command, args or workingDir differs", got.Name)
	}
	// Added variables are admission's business. Explicit env takes precedence
	// over envFrom, and the last duplicate wins, so every occurrence of an
	// operator-set name must carry the operator's value.
	for _, w := range want.Env {
		found := false
		for _, e := range got.Env {
			if e.Name != w.Name {
				continue
			}
			if !equality.Semantic.DeepEqual(e, w) {
				return fmt.Errorf("container %q overrides environment %s", got.Name, w.Name)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("container %q is missing environment %s", got.Name, w.Name)
		}
	}
	for _, w := range want.VolumeMounts {
		if !slices.ContainsFunc(got.VolumeMounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, w) }) {
			return fmt.Errorf("container %q mount %s differs", got.Name, w.MountPath)
		}
	}
	// An added mount may not shadow or nest inside the operator's paths: a mount
	// under the data directory would divert writes off the proved disk.
	for _, m := range got.VolumeMounts {
		if slices.ContainsFunc(want.VolumeMounts, func(w corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, w) }) {
			continue
		}
		for _, w := range want.VolumeMounts {
			if overlaps(m.MountPath, w.MountPath) {
				return fmt.Errorf("container %q mount %s overlaps operator mount %s", got.Name, m.MountPath, w.MountPath)
			}
		}
	}
	return nil
}

// sameArtifact accepts a registry-rewritten reference to identical content: a
// digest-pinned reference must keep its digest, anything else must be exact.
func sameArtifact(got, want string) bool {
	if i := strings.LastIndex(want, "@sha256:"); i >= 0 {
		j := strings.LastIndex(got, "@sha256:")
		return j >= 0 && got[j:] == want[i:]
	}
	return got == want
}

func overlaps(a, b string) bool {
	a, b = strings.TrimRight(a, "/"), strings.TrimRight(b, "/")
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
