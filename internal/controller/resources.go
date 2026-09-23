package controller

import (
	"fmt"
	"net/url"
	"os"
	"strconv"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	FleetLabel = "celld.eric.dev/fleet-uid"
	Finalizer  = "celld.eric.dev/lifecycle-protection"
)

type Options struct {
	OperatorNamespace string
	LauncherImage     string

	// Explicit test-only configuration; never inferred from kubeconfig or AWS environment.
	LocalTest bool
	// FaultPoint names a lifecycle boundary at which the disposable harness manager
	// exits (see faultPoint). Ignored unless LocalTest is set.
	FaultPoint string
	// LocalRWOP requests ReadWriteOncePod claims in local test mode, served by the
	// per-node hostpath CSI driver, so the shipped access mode is exercised on kind.
	// Ignored unless LocalTest is set. It is not EBS or attachment qualification.
	LocalRWOP bool
}

// localCSIDriver is the kubernetes-csi hostpath driver the harness deploys per node.
const localCSIDriver = "hostpath.csi.k8s.io"

// faultPoint terminates the manager at a named lifecycle boundary so the kind
// harness can prove crash-consistency of the current operation and workload CAS. It is a
// test-only injector honored solely with --local-test; controller-runtime
// recovers panics, so a real process exit is required.
func (r *Reconciler) faultPoint(name string) {
	if !r.Options.LocalTest || r.Options.FaultPoint != name {
		return
	}
	fmt.Fprintf(os.Stderr, "celld-operator: injected crash at lifecycle fault point %q\n", name)
	os.Exit(3)
}

func labels(f *fleet.CelldFleet) map[string]string {
	return map[string]string{FleetLabel: string(f.UID)}
}
func selector(f *fleet.CelldFleet) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: labels(f)}
}
func metadata(f *fleet.CelldFleet, name string) metav1.ObjectMeta {
	// No owner reference: foreground CR deletion must not cascade before the lifecycle gate.
	return metav1.ObjectMeta{Name: name, Namespace: f.Namespace, Labels: labels(f)}
}

func podTemplate(f *fleet.CelldFleet, opts Options) corev1.PodTemplateSpec {
	s := f.Spec
	execution := s.EffectiveExecution()
	lifecycle := s.EffectiveLifecycle()
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(execution.CPURequest), corev1.ResourceMemory: resource.MustParse(execution.MemoryRequest)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(execution.MemoryLimit)},
	}
	if execution.CPULimit != "" {
		resources.Limits[corev1.ResourceCPU] = resource.MustParse(execution.CPULimit)
	}
	mode := "bucket"
	nodeField := "metadata.uid"
	advertise := "$(POD_IP):8081"
	if s.Profile == "PersistentFleet" {
		mode = "fleet"
		nodeField = "metadata.name"
		// Predecessor recovery consults the previous lease's peer address before
		// publishing a new lease. Stable ordinal DNS reaches retained follower
		// disks even when replacement Pods receive different IPs. The peers
		// Service publishes addresses before readiness for this recovery path.
		advertise = "$(CELLD_NODE)." + f.Name + "-peers." + f.Namespace + ".svc:8081"
	}
	env := []corev1.EnvVar{
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "CELLD_NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: nodeField}}},
		{Name: "CELLD_ADVERTISE", Value: advertise},
		{Name: "CELLD_BUCKET", Value: "s3://" + s.Storage.Bucket},
		{Name: "AWS_REGION", Value: s.Storage.Region},
		{Name: "CELLD_DURABILITY", Value: mode},
		{Name: "CELLD_ADDR", Value: "0.0.0.0:8080"},
		{Name: "CELLD_INTERNAL_ADDR", Value: "0.0.0.0:8081"},
		{Name: "CELLD_WATCH", Value: "/work"},
		{Name: "CELLD_TTL_MS", Value: "10000"},
		{Name: "CELLD_SHUTDOWN_TOTAL_MS", Value: strconv.Itoa(int(lifecycle.ShutdownSeconds) * 1000)},
		{Name: "CELLD_TOKIO_THREADS", Value: "2"},
	}
	if execution.MaxResidentCells > 0 {
		env = append(env, corev1.EnvVar{Name: "CELLD_MAX_RESIDENT_CELLS", Value: strconv.Itoa(int(execution.MaxResidentCells))})
	}
	if execution.IdleEvictSeconds > 0 {
		env = append(env, corev1.EnvVar{Name: "CELLD_IDLE_EVICT_S", Value: strconv.Itoa(int(execution.IdleEvictSeconds))})
	}
	if opts.LocalTest {
		env = append(env, corev1.EnvVar{Name: "S3_ENDPOINT", Value: "http://minio.celld-test-store.svc:9000"}, corev1.EnvVar{Name: "AWS_ALLOW_HTTP", Value: "true"}, corev1.EnvVar{Name: "AWS_ACCESS_KEY_ID", Value: "qualification"}, corev1.EnvVar{Name: "AWS_SECRET_ACCESS_KEY", Value: "qualification-only"})
	}
	for _, entry := range s.Env {
		v := corev1.EnvVar{Name: entry.Name}
		if entry.Value != nil {
			v.Value = *entry.Value
		} else if entry.SecretKeyRef != nil {
			ref := &corev1.SecretKeySelector{Key: entry.SecretKeyRef.Key}
			ref.Name = entry.SecretKeyRef.Name
			v.ValueFrom = &corev1.EnvVarSource{SecretKeyRef: ref}
		}
		env = append(env, v)
	}
	if t := s.Telemetry; t != nil {
		env = append(env, corev1.EnvVar{Name: "CELLD_OTEL", Value: t.CollectorURL})
		if t.Sampler != "" {
			env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER", Value: t.Sampler})
		}
		if t.SamplerArg != "" {
			env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER_ARG", Value: t.SamplerArg})
		}
		if t.FlushMilliseconds > 0 {
			env = append(env, corev1.EnvVar{Name: "CELLD_OTEL_FLUSH_MS", Value: strconv.FormatInt(t.FlushMilliseconds, 10)})
		}
		if t.FlushBytes > 0 {
			env = append(env, corev1.EnvVar{Name: "CELLD_OTEL_FLUSH_BYTES", Value: strconv.FormatInt(t.FlushBytes, 10)})
		}
		if t.HeadersSecretKeyRef != nil {
			ref := &corev1.SecretKeySelector{Key: t.HeadersSecretKeyRef.Key}
			ref.Name = t.HeadersSecretKeyRef.Name
			env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref}})
		}
	}
	spread := corev1.TopologySpreadConstraint{
		MaxSkew:            1,
		TopologyKey:        corev1.LabelTopologyZone,
		WhenUnsatisfiable:  corev1.DoNotSchedule,
		LabelSelector:      selector(f),
		MinDomains:         new(s.Placement.AZCount),
		NodeAffinityPolicy: ptr.To(corev1.NodeInclusionPolicyHonor),
		NodeTaintsPolicy:   ptr.To(corev1.NodeInclusionPolicyHonor),
	}
	if s.Placement.Mode == "Relaxed" {
		spread.WhenUnsatisfiable = corev1.ScheduleAnyway
		spread.MinDomains = nil
	}
	pod := corev1.PodSpec{
		ServiceAccountName:            s.ServiceAccountName,
		AutomountServiceAccountToken:  new(false),
		TerminationGracePeriodSeconds: new(int64(lifecycle.TerminationGraceSeconds)),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:      new(int64(10001)),
			RunAsGroup:     new(int64(10001)),
			RunAsNonRoot:   new(true),
			FSGroup:        new(int64(10001)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: s.Placement.Zones},
								{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
							},
						},
					},
				},
			},
		},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{spread},
		Containers: []corev1.Container{
			{
				Name:            "celld",
				Image:           runtimeImage(f),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             env,
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Ports:           []corev1.ContainerPort{{Name: "application", ContainerPort: 8080}, {Name: "peer", ContainerPort: 8081}},
				ReadinessProbe: &corev1.Probe{
					HTTPGet:          &corev1.HTTPGetAction{Path: "/.well-known/celld/health", Port: intstr.FromInt32(8080)},
					PeriodSeconds:    2,
					TimeoutSeconds:   1,
					FailureThreshold: 3,
					SuccessThreshold: 1,
				},
				Resources:    resources,
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/work"}},
			},
		},
	}
	hostTerm := corev1.PodAffinityTerm{LabelSelector: selector(f), TopologyKey: corev1.LabelHostname}
	pod.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	if s.Placement.Mode == "Relaxed" {
		pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: hostTerm}}
	} else {
		pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{hostTerm}
	}
	if s.Profile == "Bucket" {
		size := resource.MustParse(fmt.Sprintf("%dGi", s.Storage.SizeGiB))
		pod.Volumes = []corev1.Volume{{Name: "data", EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}}
		pod.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = size
		pod.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage] = size
	}
	if opts.LauncherImage != "" {
		if s.Profile == "PersistentFleet" {
			pod.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
		}
		pod.InitContainers = []corev1.Container{{Name: "install-launcher", Image: opts.LauncherImage, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"/celld-launcher", "install", "/launcher/celld-launcher"}, VolumeMounts: []corev1.VolumeMount{{Name: "launcher", MountPath: "/launcher"}}, SecurityContext: pod.Containers[0].SecurityContext.DeepCopy()}}
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "launcher", EmptyDir: &corev1.EmptyDirVolumeSource{}}, corev1.Volume{Name: "launcher-key", Secret: &corev1.SecretVolumeSource{SecretName: launcherSecretName(f), DefaultMode: new(int32(0o440))}})
		c := &pod.Containers[0]
		c.Command = []string{"/launcher/celld-launcher"}
		c.Env = append(c.Env, corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}, corev1.EnvVar{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}})
		if lifecycle.TerminationGraceSeconds != fleet.DefaultTerminationGrace {
			// Hand the launcher the pod's grace period; it splits that into the
			// SIGTERM wait and the inherited-lock proof, leaving a five second
			// margin, so an unrequested termination finishes before kubelet's
			// SIGKILL. A fleet on the default grace is told nothing and the
			// launcher assumes the same 30 seconds this template sets.
			c.Env = append(c.Env, corev1.EnvVar{Name: "LAUNCHER_TERMINATION_GRACE_SECONDS", Value: strconv.Itoa(int(lifecycle.TerminationGraceSeconds))})
		}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "launcher", MountPath: "/launcher", ReadOnly: true}, corev1.VolumeMount{Name: "launcher-key", MountPath: "/launcher-key", ReadOnly: true})
		c.Ports = append(c.Ports, corev1.ContainerPort{Name: "launcher", ContainerPort: 8083})
	}
	if orderedBucket(f) {
		pod.SchedulingGates = []corev1.PodSchedulingGate{{Name: bucketZoneGate}}
		if s.Placement.Mode != "Relaxed" {
			pod.TopologySpreadConstraints = nil
		}
	}
	return corev1.PodTemplateSpec{Labels: labels(f), Spec: pod}
}

func workload(f *fleet.CelldFleet, opts Options) client.Object {
	template := podTemplate(f, opts)
	if f.Spec.Profile == "Bucket" && !orderedBucket(f) {
		return &appsv1.Deployment{
			ObjectMeta: metadata(f, f.Name),
			Spec: appsv1.DeploymentSpec{
				Replicas: new(f.Spec.Replicas),
				Selector: selector(f),
				Template: template,
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			},
		}
	}
	if orderedBucket(f) {
		// Ordinal identity determines contraction targets. Parallel management
		// lets coordinated maintenance remove every already-stopped, unready pod.
		return &appsv1.StatefulSet{ObjectMeta: metadata(f, f.Name), Spec: appsv1.StatefulSetSpec{Replicas: new(f.Spec.Replicas), Selector: selector(f), ServiceName: f.Name + "-peers", PodManagementPolicy: appsv1.ParallelPodManagement, PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType, WhenScaled: appsv1.RetainPersistentVolumeClaimRetentionPolicyType}, Template: template, UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}}}
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metadata(f, f.Name),
		Spec: appsv1.StatefulSetSpec{
			Replicas:            new(f.Spec.Replicas),
			Selector:            selector(f),
			ServiceName:         f.Name + "-peers",
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Template:            template,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					Name:        "data",
					Labels:      labels(f),
					Annotations: map[string]string{"celld.eric.dev/storage-reservation": reservationName(f)},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      persistentAccessModes(opts),
						StorageClassName: new(f.Spec.Storage.StorageClassName),
						Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", f.Spec.Storage.SizeGiB))}},
					},
				},
			},
		},
	}
}

func prerequisites(f *fleet.CelldFleet, opts Options) []client.Object {
	app := applicationService(f)
	peers := &corev1.Service{
		ObjectMeta: metadata(f, f.Name+"-peers"),
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 labels(f),
			Ports:                    []corev1.ServicePort{{Name: "peer", Port: 8081, TargetPort: intstr.FromInt32(8081)}},
		},
	}
	same := networkingv1.NetworkPolicyPeer{PodSelector: selector(f)}
	operator := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": opts.OperatorNamespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "celld-operator"}},
	}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := func(p int32) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: new(intstr.FromInt32(p))}
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metadata(f, f.Name),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *selector(f),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: []networkingv1.NetworkPolicyPeer{same, operator}, Ports: []networkingv1.NetworkPolicyPort{port(8081)}},
				{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"celld.eric.dev/client-of": f.Name}}}},
					Ports: []networkingv1.NetworkPolicyPort{port(8080)},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{To: []networkingv1.NetworkPolicyPeer{same}, Ports: []networkingv1.NetworkPolicyPort{port(8081)}},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
							PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{port(53), {Protocol: &udp, Port: new(intstr.FromInt32(53))}},
				},
				// HTTPS for S3 and STS. IAM must restrict bucket access independently.
				{Ports: []networkingv1.NetworkPolicyPort{port(443)}},
				{
					To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "169.254.170.23/32"}}},
					Ports: []networkingv1.NetworkPolicyPort{port(80)},
				},
			},
		},
	}
	if opts.LauncherImage != "" {
		policy.Spec.Ingress = append(policy.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{operator}, Ports: []networkingv1.NetworkPolicyPort{port(8083)}})
	}
	if opts.LocalTest {
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "celld-test-store"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "minio"}},
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{port(9000)},
		})
	}
	if t := f.Spec.Telemetry; t != nil {
		u, _ := url.Parse(t.CollectorURL) // validated before provisioning
		p := int32(80)
		if u.Scheme == "https" {
			p = 443
		}
		if u.Port() != "" {
			parsed, _ := strconv.Atoi(u.Port())
			p = int32(parsed)
		}
		peer := networkingv1.NetworkPolicyPeer{}
		if t.Egress.CIDR != "" {
			peer.IPBlock = &networkingv1.IPBlock{CIDR: t.Egress.CIDR}
		} else {
			peer.PodSelector = &metav1.LabelSelector{MatchLabels: t.Egress.PodLabels}
			if t.Egress.Namespace != "" {
				peer.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": t.Egress.Namespace}}
			}
		}
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer}, Ports: []networkingv1.NetworkPolicyPort{port(p)}})
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metadata(f, f.Name),
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: new(intstr.FromInt32(0)), Selector: selector(f)},
	}
	return []client.Object{policy, pdb, app, peers}
}

func applicationService(f *fleet.CelldFleet) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metadata(f, f.Name),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels(f),
			Ports:    []corev1.ServicePort{{Name: "application", Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
}
