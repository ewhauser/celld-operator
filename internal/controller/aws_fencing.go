package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/fencing"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const fenceRequestKey = "celld.eric.dev/fence-operation"

// The donor launcher must be continuously unreachable for this long before the
// fence annotation may record new termination intent. callLauncher has a short
// timeout, so a single failure is an ordinary transient, not a dead host.
const fenceUnreachableWindow = 60 * time.Second

type infrastructureFence struct {
	Operation             string
	Member                persistentMember
	Binding               fencing.Binding
	IntentAt, ConfirmedAt time.Time
}

func validInfrastructureReceipt(receipt infrastructureFence) error {
	b, m := receipt.Binding, receipt.Member
	if b.Validate() != nil || receipt.Operation == "" || receipt.IntentAt.IsZero() || (!receipt.ConfirmedAt.IsZero() && receipt.ConfirmedAt.Before(receipt.IntentAt)) || b.Volume != m.VolumeHandle || b.Zone != m.Zone || b.HostUID != m.HostUID || b.BootID != m.BootID || m.ProviderID != "aws:///"+b.Zone+"/"+b.Instance || m.DiskID == "" {
		return errors.New("invalid durable infrastructure fence receipt")
	}
	return nil
}

// certifiedInfrastructureFence is exclusively a receipt for an exact old
// invocation; it grants neither recovery completeness nor permission to move data.
func certifiedInfrastructureFence(j *lifecycleJournal, member persistentMember) bool {
	return slices.ContainsFunc(j.InfrastructureFences, func(f infrastructureFence) bool {
		return !f.IntentAt.IsZero() && !f.ConfirmedAt.IsZero() && !f.ConfirmedAt.Before(f.IntentAt) && sameInvocation(f.Member, member) && f.Member.ProviderID == member.ProviderID && validInfrastructureReceipt(f) == nil
	})
}

func (r *Reconciler) ensureInfrastructureFence(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, member persistentMember, operation string) (bool, error) {
	if certifiedInfrastructureFence(j, member) {
		return true, nil
	}
	if r.Infrastructure == nil || r.Options.LocalTest || r.Options.FencingAccount == "" || r.Options.FencingRegion == "" {
		return false, errors.New("EC2 infrastructure fencing is disabled")
	}
	if j.Loss != "" || f.Spec.Profile != "PersistentFleet" || operation == "" || f.Annotations[fenceRequestKey] != operation {
		return false, errors.New("exact operation infrastructure fencing authorization required")
	}
	if member.ProviderID == "" || member.BootID == "" || member.DiskID == "" {
		return false, errors.New("writer predates exact instance admission; cannot infer fencing identity")
	}
	prefix := "aws:///" + member.Zone + "/"
	if !strings.HasPrefix(member.ProviderID, prefix) {
		return false, errors.New("admitted AWS provider identity is invalid")
	}
	binding := fencing.Binding{Account: r.Options.FencingAccount, Region: r.Options.FencingRegion, Zone: member.Zone, Instance: strings.TrimPrefix(member.ProviderID, prefix), FleetUID: string(f.UID), HostUID: member.HostUID, BootID: member.BootID, Volume: member.VolumeHandle}
	if err := binding.Validate(); err != nil {
		return false, err
	}
	if f.Spec.Storage.Region != binding.Region {
		return false, errors.New("fencing and storage regions differ")
	}
	index := slices.IndexFunc(j.InfrastructureFences, func(item infrastructureFence) bool {
		return item.Operation == operation && sameInvocation(item.Member, member)
	})
	if index >= 0 && j.InfrastructureFences[index].Binding != binding {
		return false, errors.New("durable fencing scope changed")
	}
	instance, err := r.Infrastructure.Describe(ctx, binding.Instance)
	if err != nil {
		return false, err
	}
	if err := fencing.Check(binding, instance, index >= 0); err != nil {
		return false, err
	}
	// After intent, positive EC2 termination survives Kubernetes Node cleanup.
	if index >= 0 && instance.State == "terminated" {
		receipt := &j.InfrastructureFences[index]
		receipt.ConfirmedAt = r.capacityNow()
		if err := r.saveJournal(ctx, res, j); err != nil {
			receipt.ConfirmedAt = time.Time{}
			return false, err
		}
		return true, nil
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: member.Host}, node); err != nil {
		return false, err
	}
	if string(node.UID) != member.HostUID || node.Status.NodeInfo.BootID != member.BootID || node.Spec.ProviderID != member.ProviderID || node.Labels[corev1.LabelTopologyZone] != member.Zone {
		return false, errors.New("kubernetes host incarnation changed before fencing")
	}
	// Isolation is necessary because TerminateInstances affects the entire VM.
	// Only the admitted target and infrastructure DaemonSets may share this node.
	// The list spans namespaces (granted cluster-wide in config/manager/operator.yaml)
	// but is narrowed server-side to this node; the reconciler's client is direct,
	// so the API server serves the spec.nodeName selector without a local index.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingFields{"spec.nodeName": member.Host}); err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		// Defense in depth: never trust the selector alone to scope termination.
		if pod.Spec.NodeName != member.Host {
			continue
		}
		if pod.Namespace == f.Namespace && pod.Name == member.Node && string(pod.UID) == member.PodUID {
			continue
		}
		owner := metav1.GetControllerOf(&pod)
		if pod.Namespace == "kube-system" && owner != nil && owner.Kind == "DaemonSet" {
			continue
		}
		return false, errors.New("node hosts another workload; refusing whole-instance termination")
	}
	retained, err := r.retainedVolumeFor(ctx, f.Namespace, member.Node)
	if err != nil {
		return false, err
	}
	claim := retained.Claim
	// Fencing is the strictest of the four revalidation sites: on top of the
	// identity triple it re-asserts the journal's own claim binding, demands
	// ReadWriteOncePod and refuses a claim that is already being deleted.
	boundInJournal := string(j.Claims[claim.Name]) == member.ClaimUID
	exclusive := slices.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod})
	if !retained.sameDisk(member) || !boundInJournal || !exclusive || !claim.DeletionTimestamp.IsZero() {
		return false, errors.New("retained volume changed before fencing")
	}
	if index < 0 {
		if j.Operation == nil || j.Operation.ID != operation || j.Operation.Phase != "Stopping" || j.Operation.TargetPod != member.Node {
			return false, errors.New("fencing requires admitted persistent contraction stop")
		}
		survivors := 0
		for _, prior := range j.Operation.PersistentMembers {
			if prior.Node == member.Node {
				continue
			}
			pod := &corev1.Pod{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: prior.Node}, pod); err != nil {
				return false, err
			}
			id, _ := podIdentity(pod)
			host := &corev1.Node{}
			if err := r.Get(ctx, client.ObjectKey{Name: prior.Host}, host); err != nil {
				return false, err
			}
			state, err := r.callLauncher(ctx, f, pod, "", "")
			if err != nil {
				return false, err
			}
			if string(pod.UID) != prior.PodUID || id != prior.Container || pod.Spec.NodeName != prior.Host || !podReady(pod) || !pod.DeletionTimestamp.IsZero() || string(host.UID) != prior.HostUID || host.Status.NodeInfo.BootID != prior.BootID || !healthyHost(host) || state.Generation != prior.Generation || state.Invocation != prior.Invocation || state.Phase != "Running" {
				return false, errors.New("survivor identity or health changed before fence admission")
			}
			survivors++
		}
		if survivors < 2 {
			return false, errors.New("infrastructure contraction fencing requires two healthy admitted survivors")
		}
		j.InfrastructureFences = append(j.InfrastructureFences, infrastructureFence{Operation: operation, Member: member, Binding: binding, IntentAt: r.capacityNow()})
		// Persist before any termination call. A crash or rejected journal CAS issues nothing.
		err := r.saveJournal(ctx, res, j)
		if err != nil {
			j.InfrastructureFences = j.InfrastructureFences[:len(j.InfrastructureFences)-1]
		}
		return false, err
	}
	if !node.Spec.Unschedulable && instance.State != "shutting-down" {
		before := node.DeepCopy()
		node.Spec.Unschedulable = true
		return false, r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if instance.State == "shutting-down" {
		return false, nil
	}
	// Retrying the same immutable instance ID is idempotent. Never resolve by node
	// name or tag search here, and never terminate a replacement instance.
	return false, r.Infrastructure.Terminate(ctx, binding.Instance)
}
