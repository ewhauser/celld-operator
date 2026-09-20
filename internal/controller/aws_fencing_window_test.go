package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fencingFlowSetup drives the real Stopping reconcile loop with a donor
// launcher whose reachability the test controls.
func fencingFlowSetup(t *testing.T) (*persistentFixture, persistentMember, *fakeInfrastructure, *bool) {
	t.Helper()
	p, m, api := infrastructureSetup(t)
	// The reconcile loop patches fleet status, so the authorization annotation
	// has to be durable rather than only present on the in-memory object.
	if err := p.r.Update(t.Context(), p.f); err != nil {
		t.Fatal(err)
	}
	// infrastructureSetup leaves local-test mode (fencing rejects it), so the
	// pods must carry the production runtime environment to stay admissible.
	pods := &corev1.PodList{}
	if err := p.r.List(t.Context(), pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].Env = slices.DeleteFunc(pod.Spec.Containers[c].Env, func(e corev1.EnvVar) bool {
				return e.Name == "S3_ENDPOINT" || e.Name == "AWS_ALLOW_HTTP" || e.Name == "AWS_ACCESS_KEY_ID" || e.Name == "AWS_SECRET_ACCESS_KEY"
			})
		}
		if err := p.r.Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := p.r.List(t.Context(), claims); err != nil {
		t.Fatal(err)
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
		if err := p.r.Update(t.Context(), claim); err != nil {
			t.Fatal(err)
		}
	}
	volumes := &corev1.PersistentVolumeList{}
	if err := p.r.List(t.Context(), volumes); err != nil {
		t.Fatal(err)
	}
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if pv.Spec.CSI != nil {
			continue
		}
		pv.Spec.HostPath = nil
		pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: fmt.Sprintf("vol-000000000000000%d", i)}
		if err := p.r.Update(t.Context(), pv); err != nil {
			t.Fatal(err)
		}
	}
	// Recapture the operation against the production-shaped fixture so the
	// survivors the journal names still match what the reconciler reads.
	state := p.states[m.Node]
	state.DiskID = m.DiskID
	p.states[m.Node] = state
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	m = members[slices.IndexFunc(members, func(x persistentMember) bool { return x.Node == m.Node })]
	p.j.Operation.PersistentMembers = members
	if err := p.r.saveJournal(t.Context(), p.res, p.j); err != nil {
		t.Fatal(err)
	}
	unreachable := new(bool)
	original := p.r.launcherCall
	p.r.launcherCall = func(ctx context.Context, f *fleet.CelldFleet, pod *corev1.Pod, op, gen string) (launcher.State, error) {
		if *unreachable && pod.Name == m.Node {
			return launcher.State{}, errors.New("context deadline exceeded")
		}
		return original(ctx, f, pod, op, gen)
	}
	return p, m, api, unreachable
}

func TestInfrastructureFenceIgnoresSingleLauncherBlip(t *testing.T) {
	p, _, api, unreachable := fencingFlowSetup(t)
	*unreachable = true
	p.step(t)
	if len(p.j.InfrastructureFences) != 0 || len(api.terminations) != 0 {
		t.Fatal("one transient launcher error recorded durable fence intent")
	}
	if p.j.Operation.Phase != "Stopping" || p.j.Operation.DonorUnreachableSince.IsZero() {
		t.Fatal("sustained unreachability not tracked durably")
	}
	c := meta.FindStatusCondition(p.f.Status.Conditions, "Blocked")
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "InfrastructureFencing" || !strings.Contains(c.Message, "sustained unreachability") {
		t.Fatalf("waiting condition not reported: %v", c)
	}
	// Still inside the window: repeated failures alone must not fence.
	p.reader.now = p.reader.now.Add(fenceUnreachableWindow - time.Second)
	p.step(t)
	if len(p.j.InfrastructureFences) != 0 || len(api.terminations) != 0 {
		t.Fatal("fence intent recorded before the sustained window elapsed")
	}
}

func TestInfrastructureFenceAfterSustainedUnreachability(t *testing.T) {
	p, m, api, unreachable := fencingFlowSetup(t)
	*unreachable = true
	p.step(t)
	since := p.j.Operation.DonorUnreachableSince
	if since.IsZero() {
		t.Fatal("unreachability not tracked")
	}
	p.reader.now = p.reader.now.Add(fenceUnreachableWindow)
	p.step(t)
	if len(p.j.InfrastructureFences) != 1 || !since.Equal(p.j.Operation.DonorUnreachableSince) {
		t.Fatalf("sustained unreachability did not record exactly one durable intent: %v", p.j.InfrastructureFences)
	}
	if len(api.terminations) != 0 {
		t.Fatal("intent must precede any EC2 mutation")
	}
	p.step(t) // cordon the exact host
	p.step(t) // request termination
	if len(api.terminations) != 1 {
		t.Fatalf("termination not requested: %v", api.terminations)
	}
	api.instance.State = "terminated"
	api.instance.Disks = nil
	p.reader.stopped = true
	p.reader.barrier = true
	p.step(t)
	if !certifiedInfrastructureFence(p.j, m) || p.j.Operation.Phase != "Retiring" {
		t.Fatalf("positive receipt did not resume retirement: %v", p.j.Operation.Phase)
	}
}

func TestInfrastructureFenceRecoveryClearsUnreachability(t *testing.T) {
	p, _, api, unreachable := fencingFlowSetup(t)
	*unreachable = true
	p.step(t)
	if p.j.Operation.DonorUnreachableSince.IsZero() {
		t.Fatal("unreachability not tracked")
	}
	p.reader.now = p.reader.now.Add(fenceUnreachableWindow / 2)
	*unreachable = false
	p.step(t)
	if !p.j.Operation.DonorUnreachableSince.IsZero() {
		t.Fatal("recovered donor left a stale unreachability marker")
	}
	// A later failure restarts the window rather than inheriting the old one.
	*unreachable = true
	p.reader.now = p.reader.now.Add(fenceUnreachableWindow)
	p.step(t)
	if len(p.j.InfrastructureFences) != 0 || len(api.terminations) != 0 {
		t.Fatal("stale unreachability authorized a fence")
	}
	if p.j.Operation.DonorUnreachableSince.IsZero() {
		t.Fatal("the new failure did not restart the window")
	}
}

func TestInfrastructureFenceIntentCompletesAfterDonorReturns(t *testing.T) {
	p, m, api, unreachable := fencingFlowSetup(t)
	*unreachable = true
	p.step(t)
	p.reader.now = p.reader.now.Add(fenceUnreachableWindow)
	p.step(t)
	if len(p.j.InfrastructureFences) != 1 {
		t.Fatal("intent not recorded")
	}
	// Intent, once durable, completes instead of reverting to same-host reuse.
	*unreachable = false
	p.step(t)
	p.step(t)
	if len(api.terminations) != 1 {
		t.Fatalf("recorded intent abandoned after the launcher answered again: %v", api.terminations)
	}
	api.instance.State = "terminated"
	api.instance.Disks = nil
	p.reader.stopped = true
	p.reader.barrier = true
	p.step(t)
	if !certifiedInfrastructureFence(p.j, m) {
		t.Fatal("recorded intent did not complete")
	}
}

func TestOperationJournalRoundTripsUnreachability(t *testing.T) {
	p, _, _, _ := fencingFlowSetup(t)
	marker := p.reader.now.Add(-30 * time.Second).UTC()
	p.j.Operation.DonorUnreachableSince = marker
	if err := p.r.saveJournal(t.Context(), p.res, p.j); err != nil {
		t.Fatal(err)
	}
	loaded, err := readJournal(p.res)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Operation.DonorUnreachableSince.Equal(marker) {
		t.Fatalf("marker lost across the journal: %v", loaded.Operation.DonorUnreachableSince)
	}
	// Journals written before this field decode with a zero marker.
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(p.res.Annotations[journalKey]), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw["Operation"].(map[string]any), "DonorUnreachableSince")
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	p.res.Annotations[journalKey] = string(encoded)
	legacy, err := readJournal(p.res)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Operation.DonorUnreachableSince.IsZero() || legacy.Operation.ID != p.j.Operation.ID {
		t.Fatal("older journal without the field did not load cleanly")
	}
}
