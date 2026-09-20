package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/catalog"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bucketCandidates covers every possible Deployment victim, not a guessed
// deletion order. A matching label is not proof of ownership or bucket posture.
// This is observational admission only; it never certifies process termination.
type bucketCandidate struct {
	Container, Host, IP, Pod string
	HostUID, Hostname, Zone  string
}

func (p *ProductionEvidence) bucketCandidates(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, opts Options) (map[types.UID]bucketCandidate, error) {
	pods, err := p.pods(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(pods) != int(j.Operation.From) {
		return nil, errors.New("deployment candidate inventory differs from applied count")
	}
	base := podTemplate(f, opts).Spec
	normalizePod(&base)
	identities := map[types.UID]bucketCandidate{}
	nodes := map[string]*corev1.Node{}
	// Every pod of a fleet shares one owner, and the client is uncached: fetch
	// each distinct owner once per call rather than once per pod.
	statefulSets := map[string]*appsv1.StatefulSet{}
	replicaSets := map[string]*appsv1.ReplicaSet{}
	for i := range pods {
		pod := &pods[i]
		node := nodes[pod.Spec.NodeName]
		if node == nil {
			node = &corev1.Node{}
			if pod.Spec.NodeName == "" {
				return nil, errors.New("bucket candidate is unscheduled")
			}
			if err := p.client.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
				return nil, err
			}
			if node.UID == "" || !node.DeletionTimestamp.IsZero() {
				return nil, errors.New("bucket candidate host identity unavailable")
			}
			nodes[pod.Spec.NodeName] = node
		}
		id, _ := podIdentity(pod)
		owner := metav1.GetControllerOf(pod)
		if id == "" || !podReady(pod) || owner == nil || owner.APIVersion != "apps/v1" || owner.UID == "" {
			return nil, errors.New("bucket candidate unready or lacks exact ownership")
		}
		if orderedBucket(f) {
			sts := statefulSets[f.Name]
			if sts == nil {
				sts = &appsv1.StatefulSet{}
				if err := p.client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: f.Name}, sts); err != nil {
					return nil, err
				}
				normalizePod(&sts.Spec.Template.Spec)
				statefulSets[f.Name] = sts
			}
			if sts.UID != j.WorkloadUID || !sts.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(base, sts.Spec.Template.Spec) || sts.Spec.PodManagementPolicy != appsv1.OrderedReadyPodManagement || len(sts.Spec.VolumeClaimTemplates) != 0 {
				return nil, errors.New("ordered Bucket workload changed")
			}
			n, err := bucketOrdinal(f, pod.Name)
			if err != nil || n >= int(j.Operation.From) || owner.Kind != "StatefulSet" || owner.Name != f.Name || owner.UID != j.WorkloadUID || j.WorkloadUID == "" {
				return nil, errors.New("ordered Bucket candidate ownership or ordinal changed")
			}
			if f.Spec.Placement.Mode != "Relaxed" && (node.Labels[corev1.LabelTopologyZone] != f.Spec.Placement.Zones[n%len(f.Spec.Placement.Zones)] || pod.Spec.NodeSelector[corev1.LabelTopologyZone] != f.Spec.Placement.Zones[n%len(f.Spec.Placement.Zones)]) {
				return nil, errors.New("ordered Bucket ordinal zone differs")
			}
		} else {
			if owner.Kind != "ReplicaSet" {
				return nil, errors.New("bucket candidate lacks ReplicaSet owner")
			}
			rs := replicaSets[owner.Name]
			if rs == nil {
				rs = &appsv1.ReplicaSet{}
				if err := p.client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: owner.Name}, rs); err != nil {
					return nil, err
				}
				normalizePod(&rs.Spec.Template.Spec)
				replicaSets[owner.Name] = rs
			}
			deployment := metav1.GetControllerOf(rs)
			if rs.UID != owner.UID || !rs.DeletionTimestamp.IsZero() || deployment == nil || deployment.APIVersion != "apps/v1" || deployment.Kind != "Deployment" || deployment.Name != f.Name || deployment.UID != j.WorkloadUID || j.WorkloadUID == "" {
				return nil, errors.New("bucket candidate owner chain changed")
			}
			if !equality.Semantic.DeepEqual(base, rs.Spec.Template.Spec) {
				return nil, errors.New("bucket candidate ReplicaSet runtime template differs")
			}
		}
		// Admission can change a Pod independently of its ReplicaSet. Verify all
		// runtime configuration, while allowing externally injected AWS credentials.
		actual := pod.Spec.Containers[0]
		expected := base.Containers[0]
		if pod.Spec.ServiceAccountName != base.ServiceAccountName || pod.Spec.HostNetwork || pod.Spec.HostPID || actual.WorkingDir != expected.WorkingDir || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || len(actual.EnvFrom) != 0 || !slices.Equal(actual.Command, expected.Command) || !slices.Equal(actual.Args, expected.Args) {
			return nil, errors.New("bucket candidate runtime invocation differs")
		}
		for _, volume := range base.Volumes {
			if !slices.ContainsFunc(pod.Spec.Volumes, func(got corev1.Volume) bool { return equality.Semantic.DeepEqual(volume, got) }) {
				return nil, errors.New("bucket candidate runtime volume differs")
			}
		}
		for _, mount := range expected.VolumeMounts {
			if !slices.ContainsFunc(actual.VolumeMounts, func(got corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(mount, got) }) {
				return nil, errors.New("bucket candidate runtime mount missing")
			}
		}
		paths := map[string]bool{}
		for _, mount := range actual.VolumeMounts {
			if paths[mount.MountPath] {
				return nil, errors.New("bucket candidate duplicate mount")
			}
			paths[mount.MountPath] = true
			if !slices.ContainsFunc(expected.VolumeMounts, func(want corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(want, mount) }) && (!mount.ReadOnly || (mount.MountPath != "/var/run/secrets/eks.amazonaws.com/serviceaccount" && mount.MountPath != "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount")) {
				return nil, errors.New("bucket candidate unqualified mount")
			}
		}
		seen := map[string]bool{}
		for _, env := range actual.Env {
			if seen[env.Name] {
				return nil, errors.New("bucket candidate has duplicate environment variables")
			}
			seen[env.Name] = true
			index := slices.IndexFunc(expected.Env, func(e corev1.EnvVar) bool { return e.Name == env.Name })
			if index >= 0 {
				want := expected.Env[index]
				// FieldRef apiVersion is defaulted on actual Pods.
				if env.ValueFrom != nil && env.ValueFrom.FieldRef != nil && env.ValueFrom.FieldRef.APIVersion == "v1" {
					env = *env.DeepCopy()
					env.ValueFrom.FieldRef.APIVersion = ""
				}
				if want.ValueFrom != nil && want.ValueFrom.FieldRef != nil {
					want = *want.DeepCopy()
					want.ValueFrom.FieldRef.APIVersion = ""
				}
				if !equality.Semantic.DeepEqual(want, env) {
					return nil, errors.New("bucket candidate runtime environment differs")
				}
			} else {
				switch env.Name {
				case "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_STS_REGIONAL_ENDPOINTS", "AWS_DEFAULT_REGION":
					// EKS identity admission; no endpoint or runtime override.
				default:
					return nil, errors.New("bucket candidate has unqualified runtime configuration")
				}
			}
		}
		for _, env := range expected.Env {
			if !seen[env.Name] {
				return nil, errors.New("bucket candidate runtime configuration missing")
			}
		}
		identities[pod.UID] = bucketCandidate{Pod: pod.Name, Container: id, Host: pod.Spec.NodeName, IP: pod.Status.PodIP, HostUID: string(node.UID), Hostname: node.Labels[corev1.LabelHostname], Zone: node.Labels[corev1.LabelTopologyZone]}
	}
	return identities, nil
}

func (p *ProductionEvidence) inspectBucket(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, opts Options) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	before, err := p.bucketCandidates(ctx, f, j, opts)
	if err != nil {
		return err
	}
	var sessions []v050.Session
	for _, s := range j.Inventory.Sessions {
		if !s.Current || s.PodUID != s.Node || before[types.UID(s.PodUID)].Container != s.Container || s.Container == "" {
			return errors.New("bucket candidate history or process association unresolved")
		}
		sessions = append(sessions, v050.Session{Node: s.Node, Generation: s.Generation, Epoch: s.Epoch})
	}
	if len(sessions) != len(before) {
		return errors.New("bucket candidate session coverage incomplete")
	}
	reader, err := p.reader(ctx, f)
	if err != nil {
		return err
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return err
	}
	observation, err := adapter.InspectBucket(ctx, reader, v050.Request{OperationID: j.Operation.ID, Sessions: sessions, InventoryComplete: true, CapturedAt: j.Inventory.CheckedAt, MaxAge: 5 * time.Second, PageBudget: 1000}, p.now)
	if err != nil {
		return err
	}
	after, err := p.bucketCandidates(ctx, f, j, opts)
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(before, after) || p.now().Before(observation.ObservedAt) || p.now().Sub(j.Inventory.CheckedAt) > 5*time.Second {
		return errors.New("bucket candidates changed or observation expired during revalidation")
	}
	return nil
}
