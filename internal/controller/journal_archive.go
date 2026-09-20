package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const archivePageBytes = 128 * 1024
const archiveMaxBytes = 16 * 1024 * 1024
const archiveIndexBytes = 200 * 1024
const archiveIdentityKey = "celld.eric.dev/journal-reservation-uid"
const archiveDigestKey = "celld.eric.dev/journal-page-digest"

type journalPage struct {
	Name, Digest string
	UID          types.UID `json:",omitempty"`
	Bytes        int
}
type journalArchive struct {
	ArchiveFormat  int
	ReservationUID types.UID
	Digest         string
	Bytes          int
	Inline         map[string]json.RawMessage
	Fields         map[string][]journalPage
}

// Large histories are stored independently, so ordinary timestamp/status changes
// do not rewrite old history pages. No archive is garbage-collected with a fleet.
func (r *Reconciler) archiveJournal(ctx context.Context, res *fleet.CelldStorageReservation, raw []byte) ([]byte, error) {
	if res.UID == "" || res.Spec.FleetNamespace == "" || len(raw) > archiveMaxBytes {
		return nil, errors.New("journal archive requires reservation identity and at most 16 MiB hydrated authority")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	archive := journalArchive{ArchiveFormat: 1, ReservationUID: res.UID, Digest: digest(canonical), Bytes: len(canonical), Inline: map[string]json.RawMessage{}, Fields: map[string][]journalPage{}}
	for field, value := range fields {
		if len(value) <= 32*1024 {
			archive.Inline[field] = value
			continue
		}
		for offset := 0; offset < len(value); offset += archivePageBytes {
			end := min(offset+archivePageBytes, len(value))
			data := value[offset:end]
			hash := digest(data)
			name := "cjf-" + digest([]byte(res.UID))[:16] + "-" + hash[:40]
			page := &corev1.ConfigMap{Name: name, Namespace: res.Spec.FleetNamespace, Annotations: map[string]string{archiveIdentityKey: string(res.UID), archiveDigestKey: hash}, Immutable: new(true), BinaryData: map[string][]byte{"journal": data}}
			found := &corev1.ConfigMap{}
			err := r.Get(ctx, client.ObjectKeyFromObject(page), found)
			if apierrors.IsNotFound(err) {
				err = r.Create(ctx, page)
				if apierrors.IsAlreadyExists(err) {
					err = r.Get(ctx, client.ObjectKeyFromObject(page), found)
				} else if err == nil {
					found = page
				}
			}
			if err != nil {
				return nil, err
			}
			ref := journalPage{Name: name, Digest: hash, Bytes: len(data), UID: found.UID}
			if err := validateJournalPage(res, ref, found); err != nil {
				return nil, err
			}
			archive.Fields[field] = append(archive.Fields[field], ref)
		}
	}
	envelope, err := json.Marshal(archive)
	if err != nil {
		return nil, err
	}
	if len(envelope) > archiveIndexBytes {
		return nil, errors.New("journal archive index budget exhausted")
	}
	return envelope, nil
}

func validateJournalPage(res *fleet.CelldStorageReservation, ref journalPage, page *corev1.ConfigMap) error {
	if ref.Bytes < 1 || ref.Bytes > archivePageBytes || len(ref.Digest) != 64 || ref.Name != "cjf-"+digest([]byte(res.UID))[:16]+"-"+ref.Digest[:40] || page.Name != ref.Name || page.Namespace != res.Spec.FleetNamespace || (ref.UID != "" && ref.UID != page.UID) || !page.DeletionTimestamp.IsZero() || page.Immutable == nil || !*page.Immutable || len(page.OwnerReferences) != 0 || page.Annotations[archiveIdentityKey] != string(res.UID) || page.Annotations[archiveDigestKey] != ref.Digest || len(page.Data) != 0 || len(page.BinaryData) != 1 || len(page.BinaryData["journal"]) != ref.Bytes || digest(page.BinaryData["journal"]) != ref.Digest {
		return errors.New("journal archive page identity or integrity changed")
	}
	return nil
}

func (r *Reconciler) loadJournal(ctx context.Context, res *fleet.CelldStorageReservation) (*lifecycleJournal, error) {
	raw := res.Annotations[journalKey]
	var archive journalArchive
	if err := json.Unmarshal([]byte(raw), &archive); err != nil || archive.ArchiveFormat == 0 {
		return readJournal(res)
	}
	if archive.ArchiveFormat != 1 || archive.ReservationUID != res.UID || res.UID == "" || archive.Bytes < 1 || archive.Bytes > archiveMaxBytes || archive.Inline == nil || len(archive.Fields) == 0 || len(archive.Fields) > 64 || len(archive.Inline) > 64 {
		return nil, errors.New("invalid journal archive index")
	}
	total := 0
	for _, value := range archive.Inline {
		total += len(value)
	}
	for field, pages := range archive.Fields {
		if _, duplicate := archive.Inline[field]; duplicate || len(pages) == 0 || len(pages) > archiveMaxBytes/archivePageBytes+1 {
			return nil, errors.New("invalid journal archive field")
		}
		var value []byte
		for _, ref := range pages {
			total += ref.Bytes
			if ref.Bytes < 1 || total > archiveMaxBytes {
				return nil, errors.New("journal archive hydrate budget exhausted")
			}
			page := &corev1.ConfigMap{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: res.Spec.FleetNamespace, Name: ref.Name}, page); err != nil {
				return nil, fmt.Errorf("required journal archive page unavailable: %w", err)
			}
			if err := validateJournalPage(res, ref, page); err != nil {
				return nil, err
			}
			value = append(value, page.BinaryData["journal"]...)
		}
		if !json.Valid(value) {
			return nil, errors.New("invalid archived journal field")
		}
		archive.Inline[field] = value
	}
	restored, err := json.Marshal(archive.Inline)
	if err != nil {
		return nil, err
	}
	if len(restored) != archive.Bytes || digest(restored) != archive.Digest {
		return nil, errors.New("journal archive snapshot digest mismatch")
	}
	restoredReservation := res.DeepCopy()
	restoredReservation.Annotations[journalKey] = string(restored)
	return readJournal(restoredReservation)
}

// hydratedJournal is one reconcile's single hydration of the lifecycle journal:
// the reservation it was read from, the journal itself, and the error that read
// returned. ADR 0021 phase 1 hydrates once per reconcile and hands this to
// Bucket migration, reservation matching, the lifecycle run and every status
// report, instead of each of them loading a copy of its own and re-reading
// every archive page. j is the live journal the lifecycle mutates, so a report
// projects what the pass has already written rather than a re-read snapshot.
type hydratedJournal struct {
	res *fleet.CelldStorageReservation
	j   *lifecycleJournal
	err error
}

// hydrate reads the journal once for this reconcile. res is the same object
// every saveJournal of the pass CASes against, so a consumer can never write a
// journal that was loaded from an earlier reservation resourceVersion.
func (r *Reconciler) hydrate(ctx context.Context, res *fleet.CelldStorageReservation) *hydratedJournal {
	h := &hydratedJournal{res: res}
	h.j, h.err = r.loadJournal(ctx, res)
	return h
}

// hydrated wraps the reservation and journal an executor is already holding, so
// that its reports project those instead of hydrating the journal again.
func hydrated(res *fleet.CelldStorageReservation, j *lifecycleJournal) *hydratedJournal {
	return &hydratedJournal{res: res, j: j}
}

// journalFootprint is the size of the stored journal: the encoded bytes
// saveJournal wrote (the annotation itself while it is inline, the hydrated
// size the index records once it is paged), the archive index size, and the
// number of pages behind it. It is read from the reservation annotation rather
// than re-encoded, so observing it costs nothing on a pass that writes nothing.
type journalFootprint struct{ bytes, index, pages int }

func measureJournal(res *fleet.CelldStorageReservation) journalFootprint {
	raw := res.Annotations[journalKey]
	if raw == "" {
		return journalFootprint{}
	}
	var archive journalArchive
	if err := json.Unmarshal([]byte(raw), &archive); err != nil || archive.ArchiveFormat == 0 {
		return journalFootprint{bytes: len(raw)}
	}
	measured := journalFootprint{bytes: archive.Bytes, index: len(raw)}
	for _, pages := range archive.Fields {
		measured.pages += len(pages)
	}
	return measured
}

// nearCapacity reports whether the journal has consumed half of either budget
// that fails closed: the 16 MiB hydrated cap, beyond which no journal can be
// written at all, or the 200 KiB archive index. It warns; it blocks nothing.
func (m journalFootprint) nearCapacity() bool {
	return m.bytes > archiveMaxBytes/2 || m.index > archiveIndexBytes/2
}

func journalSizeReason(m journalFootprint) string {
	if m.nearCapacity() {
		return "JournalNearCapacity"
	}
	return "JournalWithinBudget"
}

func journalSizeMessage(m journalFootprint) string {
	return fmt.Sprintf("Lifecycle journal is %d bytes of the %d byte hydrated cap; archive index %d bytes of %d across %d pages", m.bytes, archiveMaxBytes, m.index, archiveIndexBytes, m.pages)
}
