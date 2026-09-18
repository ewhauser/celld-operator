package controller

import (
	"fmt"

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
	FleetLabel = "celld.example.com/fleet-uid"
	Finalizer  = "celld.example.com/lifecycle-protection"
	Image      = "ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8"
	// Wait on every container start, including the first restart. exec makes celld PID 1.
	// This is spacing, not fencing an old process on an unreachable node.
	launch = "trap 'exit 0' TERM INT; sleep 10 & wait $!; trap - TERM INT; exec /usr/local/bin/celld"
)

type Options struct {
	OperatorNamespace string
	// Explicit test-only configuration; never inferred from kubeconfig or AWS environment.
	LocalTest bool
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
	mode := "bucket"
	nodeField := "metadata.uid"
	if s.Profile == "PersistentFleet" {
		mode = "fleet"
		nodeField = "metadata.name"
	}
	env := []corev1.EnvVar{
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "CELLD_NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: nodeField}}},
		{Name: "CELLD_ADVERTISE", Value: "$(POD_IP):8081"},
		{Name: "CELLD_BUCKET", Value: "s3://" + s.Storage.Bucket},
		{Name: "AWS_REGION", Value: s.Storage.Region},
		{Name: "CELLD_DURABILITY", Value: mode},
		{Name: "CELLD_ADDR", Value: "0.0.0.0:8080"},
		{Name: "CELLD_INTERNAL_ADDR", Value: "0.0.0.0:8081"},
		{Name: "CELLD_WATCH", Value: "/work"},
		{Name: "CELLD_TTL_MS", Value: "10000"},
		{Name: "CELLD_SHUTDOWN_TOTAL_MS", Value: "20000"},
		{Name: "CELLD_TOKIO_THREADS", Value: "2"},
	}
	if opts.LocalTest {
		env = append(env, corev1.EnvVar{Name: "S3_ENDPOINT", Value: "http://minio.celld-test-store.svc:9000"}, corev1.EnvVar{Name: "AWS_ALLOW_HTTP", Value: "true"}, corev1.EnvVar{Name: "AWS_ACCESS_KEY_ID", Value: "qualification"}, corev1.EnvVar{Name: "AWS_SECRET_ACCESS_KEY", Value: "qualification-only"})
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
		TerminationGracePeriodSeconds: new(int64(30)),
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
				Image:           Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/sh", "-c", launch},
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
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/work"}},
			},
		},
	}
	if s.Profile == "Bucket" {
		size := resource.MustParse(fmt.Sprintf("%dGi", s.Storage.SizeGiB))
		pod.Volumes = []corev1.Volume{{Name: "data", EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}}
		pod.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = size
		pod.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage] = size
	}
	return corev1.PodTemplateSpec{Labels: labels(f), Spec: pod}
}

func workload(f *fleet.CelldFleet, opts Options) client.Object {
	template := podTemplate(f, opts)
	if f.Spec.Profile == "Bucket" {
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
					Name:   "data",
					Labels: labels(f),
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: new(f.Spec.Storage.StorageClassName),
						Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", f.Spec.Storage.SizeGiB))}},
					},
				},
			},
		},
	}
}

func prerequisites(f *fleet.CelldFleet, opts Options) []client.Object {
	app := &corev1.Service{
		ObjectMeta: metadata(f, f.Name),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels(f),
			Ports:    []corev1.ServicePort{{Name: "application", Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
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
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"celld.example.com/client-of": f.Name}}}},
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
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metadata(f, f.Name),
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: new(intstr.FromInt32(0)), Selector: selector(f)},
	}
	return []client.Object{policy, pdb, app, peers}
}
