package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func archiveFixture(t *testing.T) (*Reconciler, *fleet.CelldStorageReservation, *lifecycleJournal) {
	t.Helper()
	res := &fleet.CelldStorageReservation{Name: "archive", UID: "reservation-uid", Spec: fleet.ReservationSpec{FleetNamespace: "fleet"}}
	r := setup(t, res)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(res), res); err != nil {
		t.Fatal(err)
	}
	j := &lifecycleJournal{Version: 7, RuntimeImage: Image, Initial: 3, Applied: 3, Claims: map[string]types.UID{}, Loss: "sticky loss"}
	for i := range 4000 {
		j.History = append(j.History, lifecycleCompletion{ID: fmt.Sprintf("operation-%d", i), TargetGeneration: fmt.Sprintf("generation-%d", i), Outcome: "retired without discarding authority"})
		j.Sessions = append(j.Sessions, v050.Session{Node: fmt.Sprintf("node-%d", i), Generation: fmt.Sprintf("generation-%d", i), Epoch: 1})
	}
	return r, res, j
}
func TestArchiveRoundTripRetainsFullAuthorityAndReusesPages(t *testing.T) {
	r, res, j := archiveFixture(t)
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	if len(res.Annotations[journalKey]) > 200*1024 {
		t.Fatal("unbounded reservation annotation")
	}
	if _, err := readJournal(res); err == nil {
		t.Fatal("pure reader exposed unresolved archive")
	}
	got, err := r.loadJournal(t.Context(), res)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, j) {
		t.Fatal("archive changed lifecycle authority")
	}
	pages := &corev1.ConfigMapList{}
	if err := r.List(t.Context(), pages); err != nil {
		t.Fatal(err)
	}
	count := len(pages.Items)
	if count < 2 {
		t.Fatal("fixture did not produce pages")
	}
	for _, page := range pages.Items {
		if len(page.OwnerReferences) != 0 || page.Immutable == nil || !*page.Immutable {
			t.Fatal("archive can disappear with fleet GC")
		}
	}
	j.Loss = "additional sticky evidence"
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	if err := r.List(t.Context(), pages); err != nil {
		t.Fatal(err)
	}
	if len(pages.Items) != count {
		t.Fatal("small active update rewrote history pages")
	}
	got, err = r.loadJournal(t.Context(), res)
	if err != nil || got.Loss != j.Loss || len(got.Sessions) != 4000 {
		t.Fatalf("restore: %v", err)
	}
}

func TestArchiveRejectsMissingTamperedAndReboundPages(t *testing.T) {
	for _, which := range []string{"missing", "tamper", "mutable", "foreign", "gc", "digest", "index", "duplicate", "nil-inline"} {
		t.Run(which, func(t *testing.T) {
			r, res, j := archiveFixture(t)
			if err := r.saveJournal(t.Context(), res, j); err != nil {
				t.Fatal(err)
			}
			var a journalArchive
			if err := json.Unmarshal([]byte(res.Annotations[journalKey]), &a); err != nil {
				t.Fatal(err)
			}
			var ref journalPage
			for _, refs := range a.Fields {
				ref = refs[0]
				break
			}
			page := &corev1.ConfigMap{}
			if err := r.Get(t.Context(), client.ObjectKey{Namespace: res.Spec.FleetNamespace, Name: ref.Name}, page); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "missing":
				if err := r.Delete(t.Context(), page); err != nil {
					t.Fatal(err)
				}
			case "tamper":
				page.BinaryData["journal"][0] = 'x'
			case "mutable":
				page.Immutable = new(false)
			case "foreign":
				page.Annotations[archiveIdentityKey] = "other"
			case "gc":
				page.OwnerReferences = []metav1.OwnerReference{{UID: "fleet"}}
			case "digest":
				a.Digest = "bad"
			case "index":
				a.ReservationUID = "other"
			case "duplicate":
				for field := range a.Fields {
					a.Inline[field] = json.RawMessage(`[]`)
					break
				}
			case "nil-inline":
				a.Inline = nil
			}
			if which != "missing" {
				if err := r.Update(t.Context(), page); err != nil {
					t.Fatal(err)
				}
			}
			b, _ := json.Marshal(a)
			res.Annotations[journalKey] = string(b)
			if _, err := r.loadJournal(t.Context(), res); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestArchiveCASFailureLeavesOriginalAuthorityReadable(t *testing.T) {
	r, res, j := archiveFixture(t)
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	original := res.DeepCopy()
	j.History = append(j.History, lifecycleCompletion{ID: "uncommitted"})
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*fleet.CelldStorageReservation); ok {
			return errors.New("reservation CAS conflict")
		}
		return c.Update(ctx, obj, opts...)
	}})
	if err := r.saveJournal(t.Context(), res, j); err == nil {
		t.Fatal("conflict ignored")
	}
	got, err := r.loadJournal(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 4000 {
		t.Fatal("orphan archive published uncommitted operation")
	}
}
