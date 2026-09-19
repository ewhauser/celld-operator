package controller

import (
	"context"
	"errors"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/catalog"
	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const runtimeTransitionKey = "celld.example.com/runtime-transition"

func canStopUpgrade(f *fleet.CelldFleet, j *lifecycleJournal, options Options) bool {
	return coordinatedDowntime(f) && options.LauncherImage != "" && catalog.StoppedUpgrade(j.RuntimeImage, runtimeImage(f))
}

func (r *Reconciler) beginStoppedUpgrade(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	if !canStopUpgrade(f, j, r.Options) || j.Request == nil || j.Request.Kind != "Upgrade" || j.Request.SourceImage != j.RuntimeImage || j.Request.TargetImage != runtimeImage(f) || w.GetUID() != j.WorkloadUID || j.Loss != "" || j.Operation != nil {
		return ctrl.Result{}, true, errors.New("runtime transition lacks qualified exact source and target authority")
	}
	if paused(f) || !f.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	j.Maintenance = &maintenanceOperation{ID: j.Request.ID, Kind: "Upgrade", Phase: "Capture", Coordinated: true, TargetReplicas: j.Applied, SourceImage: j.RuntimeImage, TargetImage: j.Request.TargetImage, StartedAt: r.capacityNow(), Deadline: r.capacityNow().Add(operationBudget)}
	return r.saveMaintenance(ctx, f, res, j)
}

// The durable source/target intent precedes the StatefulSet CAS. The same-object
// marker recovers a lost Update response without guessing whether it took effect.
func transitionWorkloadImage(j *lifecycleJournal, w client.Object) (string, error) {
	m := j.Maintenance
	if m == nil || m.Kind != "Upgrade" || !m.Coordinated || m.SourceImage != j.RuntimeImage || !catalog.StoppedUpgrade(m.SourceImage, m.TargetImage) {
		return "", errors.New("invalid version transition authority")
	}
	sts, ok := w.(*appsv1.StatefulSet)
	if !ok || len(sts.Spec.Template.Spec.Containers) == 0 {
		return "", errors.New("runtime transition requires persistent StatefulSet")
	}
	image := sts.Spec.Template.Spec.Containers[0].Image
	marker := sts.Annotations[runtimeTransitionKey]
	switch m.Phase {
	case "Capture", "Stopping", "Quiesced":
		if image != m.SourceImage {
			return "", errors.New("runtime changed before all-stopped boundary")
		}
	case "Empty":
		if image == m.TargetImage {
			if replicas(w) != 0 || marker != m.ID || w.GetAnnotations()[operationKey] != m.ID {
				return "", errors.New("target image lacks exact zero-pod installation authority")
			}
		} else if image != m.SourceImage {
			return "", errors.New("unqualified image at stopped boundary")
		}
	case "Resuming":
		if image != m.TargetImage || marker != m.ID {
			return "", errors.New("resume lacks installed target image authority")
		}
	default:
		return "", errors.New("invalid runtime transition phase")
	}
	return image, nil
}

// Called only after the coordinated executor has verified exact stopped/denied
// children, sealed metadata, zero replicas and an empty pod inventory.
func (r *Reconciler) installStoppedRuntime(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, w client.Object) error {
	m := j.Maintenance
	image, err := transitionWorkloadImage(j, w)
	if err != nil {
		return err
	}
	if m.Phase != "Empty" || replicas(w) != 0 || w.GetAnnotations()[operationKey] != m.ID || w.GetUID() != j.WorkloadUID {
		return errors.New("runtime installation requires exact stopped workload")
	}
	if image == m.TargetImage {
		return nil
	}
	sts := w.(*appsv1.StatefulSet)
	target := f.DeepCopy()
	target.Spec.RuntimeImage = m.TargetImage
	sts.Spec.Template.Spec.Containers[0] = workload(target, r.Options).(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0]
	sts.Annotations[runtimeTransitionKey] = m.ID
	return r.Update(ctx, sts)
}
