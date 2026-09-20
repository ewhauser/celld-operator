package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/catalog"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/recovery"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RuntimeSession is an append-only observation, not a fencing certificate.
// Pod identity is UID/container ID/restart count. Missing historical identities
// cannot be reconstructed from a successor lease; they remain unresolved.
type RuntimeSession struct {
	Node, Generation, Pod, PodUID, Container, Host string
	Epoch                                          uint64
	FirstSeen, LastSeen                            time.Time
	Current                                        bool
	Association                                    string
}
type recoveryInventory struct {
	Sessions  []RuntimeSession
	CheckedAt time.Time
	Blocker   string
}
type readerFactory func(context.Context, *fleet.CelldFleet) (v050.Reader, error)

// ProductionEvidence collects metadata even though no production removal
// executor/fence is qualified. It never uses pod absence as termination proof.
type ProductionEvidence struct {
	client client.Client
	reader readerFactory
	now    func() time.Time
}

func NewProductionEvidence(c client.Client) *ProductionEvidence {
	// The factory runs on every reconcile of every fleet, so building a fresh
	// client here would re-resolve the operator's IRSA/Pod Identity credentials
	// and open new TLS connections every few seconds. The cache keeps one
	// hardened client per region and wraps it per bucket.
	clients := recovery.NewClientCache()
	return &ProductionEvidence{client: c, now: time.Now, reader: func(_ context.Context, f *fleet.CelldFleet) (v050.Reader, error) {
		// The reconcile context is deliberately not passed: a canceled reconcile
		// must not fail the client that every later reconcile shares. Per-request
		// timeouts still apply inside S3Reader.Get/List.
		return clients.Reader(f.Spec.Storage.Bucket, f.Spec.Storage.Region) //nolint:contextcheck // detached on purpose
	}}
}
func (p *ProductionEvidence) pods(ctx context.Context, f *fleet.CelldFleet) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := p.client.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f)), client.Limit(101)); err != nil {
		return nil, err
	}
	if pods.Continue != "" || len(pods.Items) > 100 {
		return nil, errors.New("incomplete pod inventory")
	}
	return pods.Items, nil
}
func runtimeNode(f *fleet.CelldFleet, p *corev1.Pod) string {
	if f.Spec.Profile == "Bucket" {
		return string(p.UID)
	}
	return p.Name
}
func (p *ProductionEvidence) Observe(ctx context.Context, f *fleet.CelldFleet, prior recoveryInventory) (recoveryInventory, string) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out := recoveryInventory{Sessions: slices.Clone(prior.Sessions), Blocker: "RecoveryInventoryUnavailable"}
	// A failed scan never keeps yesterday's sessions marked current.
	for i := range out.Sessions {
		out.Sessions[i].Current = false
	}
	before, err := p.pods(ctx, f)
	if err != nil {
		return out, ""
	}
	reader, err := p.reader(ctx, f)
	if err != nil {
		return out, ""
	}
	adapter, err := catalog.New(runtimeImage(f))
	if err != nil {
		return out, ""
	}
	inventory, scanErr := adapter.Inventory(ctx, reader, p.now)
	after, afterErr := p.pods(ctx, f)
	for _, node := range inventory.Nodes {
		entry := RuntimeSession{Node: node.Name, Generation: node.Generation, Epoch: node.Epoch, Association: "UnknownPodGeneration"}
		for i := range before {
			pod := &before[i]
			id, started := podIdentity(pod)
			if runtimeNode(f, pod) != node.Name || id == "" || net.ParseIP(pod.Status.PodIP) == nil || node.Address != net.JoinHostPort(pod.Status.PodIP, "8081") {
				continue
			}
			// Associate only a lease sampled during this container invocation, with
			// unchanged API identity surrounding the S3 read. This is observational;
			// the authenticated runtime generation probe is outside our IAM scope.
			sampled := time.UnixMilli(node.SampledMS)
			if sampled.Before(started) || sampled.After(p.now()) || p.now().Sub(sampled) > 15*time.Second || node.ExpiresMS <= uint64(p.now().UnixMilli()) {
				continue
			}
			for k := range after {
				current := &after[k]
				currentID, _ := podIdentity(current)
				if afterErr == nil && current.UID == pod.UID && currentID == id && current.Status.PodIP == pod.Status.PodIP && current.Spec.NodeName == pod.Spec.NodeName {
					entry.Pod, entry.PodUID, entry.Container, entry.Host = pod.Name, string(pod.UID), id, pod.Spec.NodeName
					entry.Association = "ObservedUnverified"
					entry.Current = scanErr == nil
				}
			}
		}
		// A later epoch of the same observed live incarnation keeps earlier
		// epochs current; replacement never clears an unresolved old generation.
		if entry.Current {
			for i := range out.Sessions {
				old := &out.Sessions[i]
				if old.Node == entry.Node && old.Generation == entry.Generation && old.Container == entry.Container && old.Epoch <= entry.Epoch {
					old.Current = true
				}
			}
		}
		index := slices.IndexFunc(out.Sessions, func(s RuntimeSession) bool {
			return s.Node == entry.Node && s.Generation == entry.Generation && s.Epoch == entry.Epoch && s.Container == entry.Container
		})
		if index < 0 {
			entry.FirstSeen = p.now()
			entry.LastSeen = p.now()
			out.Sessions = append(out.Sessions, entry)
		} else {
			out.Sessions[index].Current = entry.Current
			out.Sessions[index].LastSeen = p.now()
		}
	}
	if scanErr != nil || afterErr != nil {
		return out, inventory.Loss
	}
	out.CheckedAt = inventory.ObservedAt
	out.Blocker = "SessionBindingUnqualified"
	for _, s := range out.Sessions {
		if !s.Current {
			out.Blocker = "HistoricalSessionUnresolved"
			break
		}
	}
	if len(inventory.Nodes) == 0 || len(before) == 0 {
		out.Blocker = "RecoveryInventoryEmpty"
	}
	return out, inventory.Loss
}

// ValidateSurvivors is shared preflight for future mode-specific executors.
// Every exact container must still be present; all required sources must be fresh.
// Demand projection assumes the entire donor could land on any one survivor,
// rather than assuming celld balances cells uniformly.
func ValidateSurvivors(p fleet.CapacityPolicy, o capacity.Observation, identities []string, donor string, now time.Time) error {
	if !capacity.LowDemand(p, o, int32(len(identities))) || o.At.After(now) || now.Sub(o.At) > capacity.Seconds(p.MaxAgeSeconds) {
		return errors.New("survivor health or capacity evidence incomplete")
	}
	if len(identities) < 2 || len(o.Samples) != len(identities) {
		return errors.New("no survivor capacity")
	}
	seen := map[string]bool{}
	var removed *capacity.Sample
	for i := range o.Samples {
		s := &o.Samples[i]
		if !slices.Contains(identities, s.Identity) || seen[s.Identity] {
			return errors.New("container generation changed")
		}
		seen[s.Identity] = true
		if s.Identity == donor {
			removed = s
		}
	}
	if removed == nil {
		return errors.New("donor sample missing")
	}
	for _, s := range o.Samples {
		if s.Identity == donor {
			continue
		}
		if removed.CPU >= int64(p.CPUHighMillicores)-s.CPU || max(removed.MemoryMiB, removed.RuntimeMemoryMiB) >= int64(p.MemoryHighMiB)-max(s.MemoryMiB, s.RuntimeMemoryMiB) {
			return fmt.Errorf("projected capacity exceeded on %s", s.Identity)
		}
	}
	return nil
}

// No supported EKS evidence in the current narrow read-only contract proves
// process fencing. An API Pod status (including terminated exit 0), NodeReady,
// expired lease, sealed log or missing object is never a fencing certificate.
func (*ProductionEvidence) Stopped(context.Context, *fleet.CelldFleet, *lifecycleOperation) (bool, error) {
	return false, errors.New("FencingUnqualified: require independently verified exact process termination and prevention of restart, or infrastructure node fencing; Kubernetes status and S3 leases are insufficient")
}

// NewLocalEvidence uses only the fixed disposable MinIO fixture; runtime
// workload templates must also use LocalTest. No live AWS credentials are read.
func NewLocalEvidence(c client.Client) *ProductionEvidence {
	return &ProductionEvidence{client: c, now: time.Now, reader: func(_ context.Context, f *fleet.CelldFleet) (v050.Reader, error) {
		return recovery.LocalReader(f.Spec.Storage.Bucket), nil
	}}
}
