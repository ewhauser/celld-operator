package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// These are externally persisted protocol fields, rather than exported
// controller internals. The live trace checks the records the API exposes.
const (
	liveReplacementFinalizer  = "celld.eric.dev/replacing-disk"
	liveReplacementAnnotation = "celld.eric.dev/replacement-claim"
	liveReplacementFleetLabel = "celld.eric.dev/replacement-fleet"
)

// exerciseReplacement keeps the live PVC-protection/CSI proof small enough to
// run on its own, without the longer node-failure and whole-fleet scenarios.
func (h *harness) exerciseReplacement() {
	h.scale("beta", 3)
	h.writeLedger("beta")
	h.exerciseReplacementLoss()
}

// exerciseReplacementLoss also runs in the full faults suite. The actual
// backend volume is local hostpath storage; removing its PV simulates the
// cluster reporting a lost volume, rather than qualifying a cloud disk loss.
func (h *harness) exerciseReplacementLoss() {
	const podName, claimName = "beta-0", "data-beta-0"
	var trace *replacementTrace
	defer func() {
		if trace != nil {
			trace.stop()
			trace.report()
		}
	}()
	h.fault("lost volume with Pod replacement handoff", func() {
		trace = h.watchReplacement(podName, claimName)
		pv := h.faultBefore.claims[claimName].Volume
		assert(pv != "", "%s has no bound PV before loss", claimName)
		h.k("delete", "pv", pv, "--wait=false")
		h.k("patch", "pv", pv, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		h.waitReplacedDisk(claimName, 10*time.Minute)
	}, h.freshDiskAfter(claimName), h.newPod(podName), func() {
		// Settled alone would accept a leaked hold on a live Pod. Check every
		// current member explicitly, including unrelated ordinals.
		for _, p := range h.memberPods("beta") {
			assert(!slices.Contains(strs(p, "metadata", "finalizers"), liveReplacementFinalizer), "%s leaked a replacement finalizer", nameOf(p))
			assert(str(p, "metadata", "annotations", liveReplacementAnnotation) == "", "%s leaked a replacement record", nameOf(p))
			assert(str(p, "metadata", "labels", liveReplacementFleetLabel) == "", "%s leaked a replacement index label", nameOf(p))
		}
		indexed := h.listIn("pods", "-l", liveReplacementFleetLabel+"="+uidOf(h.get("celldfleet", "beta")))
		assert(len(indexed) == 0, "replacement index still discovers held Pods: %s", encode(indexed))
		old := h.faultBefore.claims[claimName]
		claim := h.get("pvc", claimName)
		assert(!terminating(claim) && str(claim, "status", "phase") == "Bound", "%s did not finish PVC protection and rebind", claimName)
		assert(uidOf(claim) != old.UID, "%s still has its old UID", claimName)
		// Each per-resource watch preserves API event order. Drain through
		// the successor on both before reading the trace; no cross-resource
		// ordering is inferred from opaque resourceVersion strings.
		h.waitFor("replacement watches observe the new Pod and claim", 30*time.Second, func() bool {
			return trace.sawSuccessors()
		})
		trace.stop()
	})
}

type replacementEvent struct {
	Kind            string    `json:"kind"`
	Event           string    `json:"event"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	Deleting        bool      `json:"deleting"`
	Finalizers      []string  `json:"finalizers,omitempty"`
	Intent          string    `json:"intent,omitempty"`
	FleetIndex      string    `json:"fleetIndex,omitempty"`
}

func replacementPodEvent(p *corev1.Pod, event string) replacementEvent {
	return replacementEvent{Kind: "Pod", Event: event, Name: p.Name, UID: p.UID, ResourceVersion: p.ResourceVersion,
		Deleting: !p.DeletionTimestamp.IsZero(), Finalizers: slices.Clone(p.Finalizers), Intent: p.Annotations[liveReplacementAnnotation], FleetIndex: p.Labels[liveReplacementFleetLabel]}
}

func replacementClaimEvent(c *corev1.PersistentVolumeClaim, event string) replacementEvent {
	return replacementEvent{Kind: "PVC", Event: event, Name: c.Name, UID: c.UID, ResourceVersion: c.ResourceVersion,
		Deleting: !c.DeletionTimestamp.IsZero(), Finalizers: slices.Clone(c.Finalizers)}
}

type replacementTrace struct {
	mu                     sync.Mutex
	events                 []replacementEvent
	issues                 []string
	oldPodUID, oldClaimUID types.UID
	oldFleetUID            types.UID
	done                   chan struct{}
	cancel                 context.CancelFunc
	pods, claims           watch.Interface
	stopOnce               sync.Once
}

func (h *harness) watchReplacement(podName, claimName string) *replacementTrace {
	// Explicit loading rules and context keep this independent of the user's
	// default kubeconfig, just like every kubectl command in this harness.
	loading := &clientcmd.ClientConfigLoadingRules{ExplicitPath: h.kubeconfig}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + h.name}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
	must(err)
	client, err := kubernetes.NewForConfig(config)
	must(err)
	ctx, cancel := context.WithCancel(h.ctx)
	failed := true
	defer func() {
		if failed {
			cancel()
		}
	}()
	podOptions := metav1.ListOptions{FieldSelector: "metadata.name=" + podName}
	claimOptions := metav1.ListOptions{FieldSelector: "metadata.name=" + claimName}
	podList, err := client.CoreV1().Pods("fleets").List(ctx, podOptions)
	must(err)
	claimList, err := client.CoreV1().PersistentVolumeClaims("fleets").List(ctx, claimOptions)
	must(err)
	assert(len(podList.Items) == 1 && len(claimList.Items) == 1, "replacement watch requires its original Pod and claim")
	assert(slices.Contains(claimList.Items[0].Finalizers, "kubernetes.io/pvc-protection"), "%s has no PVC-protection finalizer before loss", claimName)
	t := &replacementTrace{oldPodUID: podList.Items[0].UID, oldClaimUID: claimList.Items[0].UID,
		oldFleetUID: types.UID(podList.Items[0].Labels["celld.eric.dev/fleet-uid"]), done: make(chan struct{}), cancel: cancel}
	t.events = []replacementEvent{replacementPodEvent(&podList.Items[0], "Initial"), replacementClaimEvent(&claimList.Items[0], "Initial")}
	podOptions.ResourceVersion = podList.ResourceVersion
	claimOptions.ResourceVersion = claimList.ResourceVersion
	t.pods, err = client.CoreV1().Pods("fleets").Watch(ctx, podOptions)
	must(err)
	t.claims, err = client.CoreV1().PersistentVolumeClaims("fleets").Watch(ctx, claimOptions)
	if err != nil {
		t.pods.Stop()
		must(err)
	}
	failed = false
	go t.collect(ctx, client, podName)
	return t
}

func (t *replacementTrace) add(e replacementEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}

func (t *replacementTrace) issue(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.issues = append(t.issues, fmt.Sprintf(format, args...))
}

func (t *replacementTrace) collect(ctx context.Context, client kubernetes.Interface, podName string) {
	defer close(t.done)
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-t.pods.ResultChan():
			if !ok {
				if ctx.Err() == nil {
					t.issue("Pod watch closed before completion")
				}
				return
			}
			if p, ok := e.Object.(*corev1.Pod); ok {
				t.add(replacementPodEvent(p, string(e.Type)))
			} else {
				t.issue("Pod watch event %s: %v", e.Type, e.Object)
			}
		case e, ok := <-t.claims.ResultChan():
			if !ok {
				if ctx.Err() == nil {
					t.issue("PVC watch closed before completion")
				}
				return
			}
			c, ok := e.Object.(*corev1.PersistentVolumeClaim)
			if !ok {
				t.issue("PVC watch event %s: %v", e.Type, e.Object)
				continue
			}
			t.add(replacementClaimEvent(c, string(e.Type)))
			if c.UID == t.oldClaimUID && !c.DeletionTimestamp.IsZero() && e.Type == watch.Modified {
				// A successful read of the held old Pod after this watch event
				// proves the hold persisted after claim deletion was accepted.
				// The protocol can release it before this GET arrives; that is
				// an uncaptured short phase, not a failure of CSI completion.
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				p, err := client.CoreV1().Pods("fleets").Get(readCtx, podName, metav1.GetOptions{})
				cancel()
				if err == nil {
					t.add(replacementPodEvent(p, "ReadAfterClaimDelete"))
				} else if !apierrors.IsNotFound(err) && ctx.Err() == nil {
					t.issue("Pod read after claim delete: %v", err)
				}
			}
		}
	}
}

func (t *replacementTrace) sawSuccessors() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	var pod, claim bool
	for _, e := range t.events {
		pod = pod || e.Kind == "Pod" && e.UID != t.oldPodUID && e.Event == string(watch.Added)
		claim = claim || e.Kind == "PVC" && e.UID != t.oldClaimUID && e.Event == string(watch.Added)
	}
	return pod && claim
}

func (t *replacementTrace) stop() {
	t.stopOnce.Do(func() {
		t.cancel()
		t.pods.Stop()
		t.claims.Stop()
		<-t.done
	})
}

func (t *replacementTrace) report() {
	// stop has joined the only writer, so this final report requires no lock.
	var prepared, committed, holdAfterClaimDelete bool
	for _, e := range t.events {
		fmt.Println("REPLACEMENT EVENT:", encode(e))
		if e.Kind != "Pod" || e.UID != t.oldPodUID || !slices.Contains(e.Finalizers, liveReplacementFinalizer) {
			continue
		}
		var intent struct {
			FleetUID  types.UID `json:"fleetUID"`
			ClaimUID  types.UID `json:"claimUID"`
			Committed bool      `json:"committed"`
		}
		must(json.Unmarshal([]byte(e.Intent), &intent))
		assert(intent.ClaimUID == t.oldClaimUID, "held Pod references claim %s rather than %s", intent.ClaimUID, t.oldClaimUID)
		assert(intent.FleetUID == t.oldFleetUID && e.FleetIndex == string(t.oldFleetUID), "held Pod's recorded/indexed fleet differs from %s", t.oldFleetUID)
		prepared = prepared || !intent.Committed
		committed = committed || intent.Committed && e.Deleting
		holdAfterClaimDelete = holdAfterClaimDelete || e.Event == "ReadAfterClaimDelete" && intent.Committed && e.Deleting
	}
	for _, issue := range t.issues {
		fmt.Println("REPLACEMENT WATCH LIMIT:", issue)
	}
	fmt.Printf("REPLACEMENT WATCH: prepared=%t committed=%t held-after-claim-delete=%t\n", prepared, committed, holdAfterClaimDelete)
	if !holdAfterClaimDelete {
		fmt.Println("REPLACEMENT WATCH LIMIT: the short hold after PVC deletion was accepted was not captured; fresh CSI identity and no leaked hold were checked")
	}
}
