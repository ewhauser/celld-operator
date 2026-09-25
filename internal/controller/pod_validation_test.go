package controller

import (
	"slices"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Admission shapes modeled on common injectors. They are qualification
// fixtures only: the validator names none of them.
func istio(p *corev1.PodSpec) {
	p.InitContainers = append(p.InitContainers, corev1.Container{Name: "istio-init", Image: "proxyv2:fixture", Args: []string{"istio-iptables"}, SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"}}}})
	always := corev1.ContainerRestartPolicyAlways
	p.InitContainers = append([]corev1.Container{{Name: "istio-proxy", Image: "proxyv2:fixture", RestartPolicy: &always, Env: []corev1.EnvVar{{Name: "ISTIO_META_POD_PORTS", Value: "[]"}}, VolumeMounts: []corev1.VolumeMount{{Name: "istio-envoy", MountPath: "/etc/istio/proxy"}, {Name: "istio-token", MountPath: "/var/run/secrets/tokens"}}}}, p.InitContainers...)
	p.Volumes = append(p.Volumes, corev1.Volume{Name: "istio-envoy", EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}, corev1.Volume{Name: "istio-token", Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "istio-ca", Path: "istio-token"}}}}})
}
func legacySidecar(p *corev1.PodSpec) {
	p.Containers = append([]corev1.Container{{Name: "istio-proxy", Image: "proxyv2:fixture", VolumeMounts: []corev1.VolumeMount{{Name: "istio-envoy", MountPath: "/etc/istio/proxy"}}}}, p.Containers...)
	p.Volumes = append(p.Volumes, corev1.Volume{Name: "istio-envoy", EmptyDir: &corev1.EmptyDirVolumeSource{}})
}
func irsa(p *corev1.PodSpec) {
	p.Volumes = append(p.Volumes, corev1.Volume{Name: "aws-iam-token", Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "sts.amazonaws.com", Path: "token"}}}}})
	for _, list := range []*[]corev1.Container{&p.Containers, &p.InitContainers} {
		for i := range *list {
			c := &(*list)[i]
			c.Env = append(c.Env, corev1.EnvVar{Name: "AWS_ROLE_ARN", Value: "arn:aws:iam::123456789012:role/runtime"}, corev1.EnvVar{Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"})
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "aws-iam-token", MountPath: "/var/run/secrets/eks.amazonaws.com/serviceaccount", ReadOnly: true})
		}
	}
}
func datadog(p *corev1.PodSpec) {
	socket := corev1.HostPathDirectoryOrCreate
	p.Volumes = append(p.Volumes, corev1.Volume{Name: "datadog", HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/datadog/", Type: &socket}})
	c := &p.Containers[len(p.Containers)-1]
	c.Env = append(c.Env, corev1.EnvVar{Name: "DD_AGENT_HOST", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}}, corev1.EnvVar{Name: "DD_ENTITY_ID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}, corev1.EnvVar{Name: "DD_ENV", Value: "development"})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "datadog", MountPath: "/var/run/datadog"})
}
func runtimeContainer(p *corev1.PodSpec) *corev1.Container {
	for i := range p.Containers {
		if p.Containers[i].Name == "celld" {
			return &p.Containers[i]
		}
	}
	panic("runtime container missing")
}
func mirrored(image string) string {
	return "mirror.example.com/cache/" + image[strings.LastIndex(image, "/")+1:]
}

func TestPersistentPodAdmissionContract(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	opts := Options{OperatorNamespace: "celld-system", LauncherImage: fixtureLauncher}
	pod := func() *corev1.Pod {
		spec := podTemplate(f, opts).Spec
		spec.SchedulingGates = nil
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-alpha-0"}})
		return &corev1.Pod{Name: "alpha-0", Namespace: f.Namespace, Spec: spec}
	}
	for _, c := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
		reject string
	}{
		{name: "template"},
		{name: "native sidecar mesh", mutate: istio},
		{name: "legacy sidecar mesh", mutate: func(p *corev1.PodSpec) { istio(p); legacySidecar(p) }},
		{name: "workload identity", mutate: irsa},
		{name: "telemetry", mutate: datadog},
		{name: "all injectors", mutate: func(p *corev1.PodSpec) { istio(p); legacySidecar(p); irsa(p); datadog(p) }},
		{name: "policy engine hardening", mutate: func(p *corev1.PodSpec) {
			runtimeContainer(p).SecurityContext.ReadOnlyRootFilesystem = new(true)
			p.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: new("profiles/runtime.json")}
		}},
		{name: "registry mirror keeps digests", mutate: func(p *corev1.PodSpec) {
			runtimeContainer(p).Image = mirrored(fixtureRuntime)
			p.InitContainers[0].Image = mirrored(fixtureLauncher)
		}},
		{name: "added envFrom", mutate: func(p *corev1.PodSpec) {
			runtimeContainer(p).EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{Name: "injected"}}}
		}},

		{name: "runtime digest", reject: `container "celld" image`, mutate: func(p *corev1.PodSpec) {
			runtimeContainer(p).Image = "ghcr.io/ewhauser/celld@sha256:" + strings.Repeat("c", 64)
		}},
		{name: "runtime digest dropped", reject: `container "celld" image`, mutate: func(p *corev1.PodSpec) {
			runtimeContainer(p).Image = "ghcr.io/ewhauser/celld:latest"
		}},
		{name: "launcher digest", reject: `container "install-launcher" image`, mutate: func(p *corev1.PodSpec) {
			p.InitContainers[0].Image = "ghcr.io/ewhauser/celld-operator@sha256:" + strings.Repeat("c", 64)
		}},
		{name: "runtime command", reject: `container "celld" command`, mutate: func(p *corev1.PodSpec) { runtimeContainer(p).Command = []string{"/bin/sh"} }},
		{name: "runtime args", reject: `container "celld" command`, mutate: func(p *corev1.PodSpec) { runtimeContainer(p).Args = []string{"--unsafe"} }},
		{name: "launcher command", reject: `container "install-launcher" command`, mutate: func(p *corev1.PodSpec) { p.InitContainers[0].Command = []string{"/bin/true"} }},
		{name: "operator env overridden", reject: `overrides environment CELLD_BUCKET`, mutate: func(p *corev1.PodSpec) {
			c := runtimeContainer(p)
			c.Env = append(c.Env, corev1.EnvVar{Name: "CELLD_BUCKET", Value: "s3://elsewhere"})
		}},
		{name: "operator env removed", reject: `missing environment CELLD_WATCH`, mutate: func(p *corev1.PodSpec) {
			c := runtimeContainer(p)
			c.Env = slices.DeleteFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "CELLD_WATCH" })
		}},
		{name: "sidecar mounts disk", reject: `container "istio-proxy" mounts operator volume "data"`, mutate: func(p *corev1.PodSpec) {
			legacySidecar(p)
			p.Containers[0].VolumeMounts = append(p.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data", ReadOnly: true})
		}},
		{name: "init mounts launcher binary", reject: `container "istio-init" mounts operator volume "launcher"`, mutate: func(p *corev1.PodSpec) {
			istio(p)
			last := &p.InitContainers[len(p.InitContainers)-1]
			last.VolumeMounts = append(last.VolumeMounts, corev1.VolumeMount{Name: "launcher", MountPath: "/launcher"})
		}},
		{name: "sidecar reads launcher key", reject: `mounts operator volume "launcher-key"`, mutate: func(p *corev1.PodSpec) {
			istio(p)
			p.InitContainers[0].VolumeMounts = append(p.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: "launcher-key", MountPath: "/key", ReadOnly: true})
		}},
		{name: "sidecar disk device", reject: `attaches operator volume "data"`, mutate: func(p *corev1.PodSpec) {
			legacySidecar(p)
			p.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "data", DevicePath: "/dev/xvdb"}}
		}},
		{name: "runtime second writable disk mount", reject: `container "celld" mounts operator volume "data"`, mutate: func(p *corev1.PodSpec) {
			c := runtimeContainer(p)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/elsewhere"})
		}},
		{name: "mount shadows data directory", reject: `mount /work/cache overlaps operator mount /work`, mutate: func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "cache", EmptyDir: &corev1.EmptyDirVolumeSource{}})
			c := runtimeContainer(p)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "cache", MountPath: "/work/cache"})
		}},
		{name: "operator mount missing", reject: `container "celld" mount /launcher-key differs`, mutate: func(p *corev1.PodSpec) {
			c := runtimeContainer(p)
			c.VolumeMounts = c.VolumeMounts[:len(c.VolumeMounts)-1]
		}},
		{name: "launcher key source", reject: `operator volume "launcher-key" differs`, mutate: func(p *corev1.PodSpec) {
			for i := range p.Volumes {
				if p.Volumes[i].Name == "launcher-key" {
					p.Volumes[i].Secret.SecretName = "attacker"
				}
			}
		}},
		{name: "other ordinal claim", reject: `exact ordinal claim`, mutate: func(p *corev1.PodSpec) {
			p.Volumes[len(p.Volumes)-1].PersistentVolumeClaim.ClaimName = "data-alpha-1"
		}},
		{name: "launcher missing", reject: `operator container "install-launcher" is missing`, mutate: func(p *corev1.PodSpec) { p.InitContainers = nil }},
		{name: "runtime missing", reject: `operator container "celld" is missing`, mutate: func(p *corev1.PodSpec) { p.Containers[0].Name = "renamed" }},
		{name: "shared process namespace", reject: `shareProcessNamespace`, mutate: func(p *corev1.PodSpec) { p.ShareProcessNamespace = new(true) }},
		{name: "host pid", reject: `host namespace`, mutate: func(p *corev1.PodSpec) { p.HostPID = true }},
		{name: "debug container", reject: `ephemeral container "debugger"`, mutate: func(p *corev1.PodSpec) {
			p.EphemeralContainers = []corev1.EphemeralContainer{{Name: "debugger"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := pod()
			if c.mutate != nil {
				c.mutate(&p.Spec)
			}
			err := validatePersistentPod(f, p, opts)
			if c.reject == "" && err != nil {
				t.Fatalf("admitted shape rejected: %v", err)
			}
			if c.reject != "" && (err == nil || !strings.Contains(err.Error(), c.reject)) {
				t.Fatalf("want rejection containing %q, got %v", c.reject, err)
			}
		})
	}
}

func TestRuntimeIdentityIgnoresInjectedContainers(t *testing.T) {
	p := &corev1.Pod{UID: "pod-uid", Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "istio-proxy", Image: "proxyv2:fixture"}, {Name: "celld", Image: fixtureRuntime}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "istio-proxy", ContainerID: "proxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}},
		{Name: "celld", ContainerID: "runtime", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}},
	}}}
	if id, _ := podIdentity(p); id != "pod-uid/runtime/0" {
		t.Fatalf("runtime identity %q", id)
	}
	p.Spec.Containers = p.Spec.Containers[:1]
	if id, _ := podIdentity(p); id != "" {
		t.Fatalf("identity without runtime container %q", id)
	}
}

func TestMaintenanceWithAdmittedPods(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, kind := range []string{"Restart", "Upgrade"} {
			t.Run(profile+"/"+kind, func(t *testing.T) {
				x := newOperationFixture(t, profile)
				x.admit = func(p *corev1.PodSpec) { istio(p); legacySidecar(p); irsa(p); datadog(p) }
				x.resetPods()
				if c := meta.FindStatusCondition(reconcile(t, x.r, x.f).Status.Conditions, "Blocked"); c == nil || c.Status != metav1.ConditionFalse {
					t.Fatalf("admitted fleet reported blocked: %+v", c)
				}
				x.edit(func(f *fleet.CelldFleet) {
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "request-1", AllowCoordinatedDowntime: true}
					if kind == "Upgrade" {
						f.Spec.RuntimeImage = "ghcr.io/ewhauser/celld@sha256:" + strings.Repeat("c", 64)
					}
				})
				x.until("Intent")
				if len(x.state().Operation.Targets) != 3 {
					t.Fatal("maintenance working set incomplete")
				}
				x.finish()
				if s := x.state(); s.Applied != 3 || s.RestartToken != "request-1" || s.Operation != nil || s.RuntimeImage != x.f.Spec.RuntimeImage {
					t.Fatal("maintenance incomplete", s)
				}
			})
		}
	}
}

func TestProvisioningReportsUnsupportedComposition(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			x := newOperationFixture(t, profile)
			x.admit = func(p *corev1.PodSpec) {
				legacySidecar(p)
				p.Containers[0].VolumeMounts = append(p.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "launcher-key", MountPath: "/key", ReadOnly: true})
			}
			x.resetPods()
			got := reconcile(t, x.r, x.f)
			c := meta.FindStatusCondition(got.Status.Conditions, "Blocked")
			if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "PodCompositionUnsupported" || !strings.Contains(c.Message, `container "istio-proxy" mounts operator volume "launcher-key"`) {
				t.Fatalf("unsupported composition not reported before maintenance: %+v", c)
			}
			if !meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
				t.Fatal("diagnostic withdrew readiness of a serving fleet")
			}
		})
	}
}

// resetPods recreates the fixture's Pods through admission, as a rollout would.
func (x *operationFixture) resetPods() {
	x.t.Helper()
	if err := x.r.DeleteAllOf(x.t.Context(), &corev1.Pod{}, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	x.syncWorkload()
}
