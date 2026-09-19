package controller

import (
	"context"
	"errors"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/fencing"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fakeInfrastructure struct {
	instance                  fencing.Instance
	terminations              []string
	describeErr, terminateErr error
}

func (f *fakeInfrastructure) Describe(context.Context, string) (fencing.Instance, error) {
	return f.instance, f.describeErr
}
func (f *fakeInfrastructure) Terminate(_ context.Context, id string) error {
	f.terminations = append(f.terminations, id)
	return f.terminateErr
}

func infrastructureSetup(t *testing.T) (*persistentFixture, persistentMember, *fakeInfrastructure) {
	t.Helper()
	p := persistentSetup(t)
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	m := members[2]
	m.ProviderID = "aws:///us-east-1a/i-0123456789abcdef0"
	m.DiskID = "disk-nonce"
	m.VolumeHandle = "vol-0123456789abcdef0"
	n := &corev1.Node{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Name: m.Host}, n); err != nil {
		t.Fatal(err)
	}
	n.Spec.ProviderID = m.ProviderID
	if err := p.r.Update(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "data-" + m.Node}, claim); err != nil {
		t.Fatal(err)
	}
	claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	if err := p.r.Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Name: claim.Spec.VolumeName}, pv); err != nil {
		t.Fatal(err)
	}
	pv.Spec.HostPath = nil
	pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: m.VolumeHandle}
	if err := p.r.Update(t.Context(), pv); err != nil {
		t.Fatal(err)
	}
	p.r.Options.LocalTest = false
	p.r.Options.FencingAccount = "123456789012"
	p.r.Options.FencingRegion = "us-east-1"
	p.f.Annotations = map[string]string{fenceRequestKey: "removal"}
	api := &fakeInfrastructure{instance: fencing.Instance{Account: "123456789012", ID: "i-0123456789abcdef0", Zone: m.Zone, State: "running", Tags: map[string]string{fencing.FleetTag: string(p.f.UID), fencing.HostTag: m.HostUID, fencing.BootTag: m.BootID, fencing.FenceTag: "terminate"}, Disks: []fencing.Disk{{ID: m.VolumeHandle}}}}
	p.r.Infrastructure = api
	members[2] = m
	p.j.Operation.PersistentMembers = members
	p.j.Operation.TargetPod = m.Node
	p.j.Operation.TargetUID = m.PodUID
	p.j.Operation.TargetGeneration = m.Generation
	p.j.Operation.Phase = "Stopping"
	return p, m, api
}
func TestInfrastructureFenceDurableIntentBeforeEffectAndPositiveReceipt(t *testing.T) {
	p, m, api := infrastructureSetup(t)
	call := func() bool {
		t.Helper()
		done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal")
		if err != nil {
			t.Fatal(err)
		}
		return done
	}
	if call() || len(api.terminations) != 0 || len(p.j.InfrastructureFences) != 1 {
		t.Fatal("first reconcile must only persist intent")
	}
	// Restart from durable reservation, rather than trusting the in-memory object.
	persisted, err := readJournal(p.res)
	if err != nil {
		t.Fatal(err)
	}
	p.j = persisted
	if call() || len(api.terminations) != 0 {
		t.Fatal("second reconcile must cordon exact host")
	}
	if call() || len(api.terminations) != 1 {
		t.Fatal("termination request is not proof")
	}
	api.instance.State = "shutting-down"
	if call() || len(api.terminations) != 1 {
		t.Fatal("shutting-down is not proof")
	}
	api.describeErr = errors.New("InvalidInstanceID.NotFound")
	if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); done || err == nil {
		t.Fatal("absence accepted as termination")
	}
	api.describeErr = nil
	api.instance.State = "terminated"
	api.instance.Disks = nil
	if !call() || !certifiedInfrastructureFence(p.j, m) {
		t.Fatal("positive terminated receipt not persisted")
	}
	different := m
	different.Generation = "replacement"
	if certifiedInfrastructureFence(p.j, different) {
		t.Fatal("fence accepted for replacement generation")
	}
}
func TestInfrastructureFenceRejectsScopeChanges(t *testing.T) {
	for _, fault := range []string{"disabled", "authorization", "ownership", "diskdelete", "provider", "reboot", "otherpod", "loss", "scopechange", "survivor"} {
		t.Run(fault, func(t *testing.T) {
			p, m, api := infrastructureSetup(t)
			switch fault {
			case "disabled":
				p.r.Infrastructure = nil
			case "authorization":
				p.f.Annotations[fenceRequestKey] = "other"
			case "ownership":
				api.instance.Tags[fencing.FleetTag] = "other"
			case "diskdelete":
				api.instance.Disks[0].DeleteOnTermination = true
			case "provider":
				m.ProviderID = "aws:///us-east-1a/i-fffffffffffffffff"
			case "reboot":
				m.BootID = "new"
			case "otherpod":
				if err := p.r.Create(t.Context(), &corev1.Pod{Name: "innocent", Namespace: "other", Spec: corev1.PodSpec{NodeName: m.Host}}); err != nil {
					t.Fatal(err)
				}
			case "survivor":
				state := p.states["persistent-0"]
				state.Invocation = "changed"
				p.states["persistent-0"] = state
			case "loss":
				p.j.Loss = "durable loss"
			case "scopechange":
				if _, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); err != nil {
					t.Fatal(err)
				}
				p.r.Options.FencingAccount = "999999999999"
			}
			if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); done || err == nil {
				t.Fatal("unsafe fence accepted")
			}
			if len(api.terminations) != 0 {
				t.Fatal("unsafe termination issued")
			}
		})
	}
}

func TestInfrastructureFenceCASFailureCannotGrantAuthority(t *testing.T) {
	for _, phase := range []string{"intent", "confirmation"} {
		t.Run(phase, func(t *testing.T) {
			p, m, api := infrastructureSetup(t)
			if phase == "confirmation" {
				if _, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); err != nil {
					t.Fatal(err)
				}
				api.instance.State = "terminated"
				api.instance.Disks = nil
			}
			p.r.Client = interceptor.NewClient(p.r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*fleet.CelldStorageReservation); ok {
					return errors.New("reservation CAS lost")
				}
				return c.Update(ctx, obj, opts...)
			}})
			for range 2 {
				if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); done || err == nil {
					t.Fatal("failed save granted authority")
				}
				if certifiedInfrastructureFence(p.j, m) || len(api.terminations) > 0 {
					t.Fatal("failed save allowed effect or receipt")
				}
			}
		})
	}
}

func TestInfrastructureReceiptRejectsCorruptBinding(t *testing.T) {
	p, m, api := infrastructureSetup(t)
	if _, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); err != nil {
		t.Fatal(err)
	}
	api.instance.State = "terminated"
	api.instance.Disks = nil
	if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); !done || err != nil {
		t.Fatal(err)
	}
	p.j.InfrastructureFences[0].Binding.Volume = "vol-fffffffffffffffff"
	if certifiedInfrastructureFence(p.j, m) {
		t.Fatal("corrupt binding became a fence")
	}
	if err := validatePersistentJournal(p.j); err == nil {
		t.Fatal("corrupt receipt accepted by journal validation")
	}
}

func TestInfrastructureFenceTerminatedReceiptSurvivesNodeRemoval(t *testing.T) {
	p, m, api := infrastructureSetup(t)
	if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); done || err != nil {
		t.Fatalf("intent: %v %v", done, err)
	}
	node := &corev1.Node{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Name: m.Host}, node); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Delete(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); done || err == nil {
		t.Fatal("missing node accepted before termination")
	}
	api.instance.State = "terminated"
	api.instance.Disks = nil
	if done, err := p.r.ensureInfrastructureFence(t.Context(), p.f, p.res, p.j, m, "removal"); !done || err != nil {
		t.Fatalf("positive receipt: %v %v", done, err)
	}
	if len(api.terminations) != 0 {
		t.Fatal("receipt lookup issued new termination")
	}
}
