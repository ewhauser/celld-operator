package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ewhauser/celld-operator/internal/launcher"
)

func (h *harness) exerciseFaults() {
	for _, f := range []struct{ name, probe string }{{"alpha", "client"}, {"beta", "client-beta"}} {
		h.writeLedger(f.probe, f.name)
		h.scale(f.name, 3)
		for _, point := range []string{"before-effect", "after-effect"} {
			workload := h.get("statefulset", f.name)
			before := generation(workload)
			target := h.captureCrashTarget(f.name, workload)
			manager := h.setOperatorFault(point)
			h.setReplicas(f.name, 2)
			h.waitFor("manager crashes at "+point+": "+f.name, 8*time.Minute, func() bool {
				out, _ := h.tryK("-n", operatorNS, "logs", manager, "-c", "operator", "--previous")
				return strings.Contains(out, "injected crash at lifecycle fault point \""+point+"\"")
			})
			state := h.currentState(f.name)
			op := sub(state, "Operation")
			must(validateCrashProof(op, target))
			expected := int64(3)
			expectedGen := before
			if point == "after-effect" {
				expected = 2
				expectedGen++
			}
			assert(specReplicas(h.get("statefulset", f.name)) == expected, "unexpected workload effect")
			h.setOperatorFault("")
			h.waitFor("manager resumes exact operation: "+f.name, 8*time.Minute, func() bool { return h.settled(f.name, 2) })
			completion := sub(h.currentState(f.name), "Completion")
			assert(str(completion, "ID") == str(op, "ID"), "manager completed a different operation")
			assert(generation(h.get("statefulset", f.name)) == before+1, "more than one workload replica write: before %d, crashed %d", before, expectedGen)
			h.readLedger(f.probe, f.name)
			h.scale(f.name, 3)
		}
		h.scale(f.name, 2)
	}
	// This is a storage transport fault, not private object decoding. Runtime state
	// remains the authority and the harness only checks count, identity and data.
	h.toxic("POST", "/proxies/minio/toxics", object{"name": "latency", "type": "latency", "stream": "downstream", "attributes": object{"latency": 250, "jitter": 50}})
	func() {
		defer h.toxic("DELETE", "/proxies/minio/toxics/latency", nil)
		h.scale("alpha", 3)
		h.scale("alpha", 2)
		h.readLedger("client", "alpha")
	}()
	h.exerciseLeaseLoss()
	fmt.Println("PASS: both profiles preserve acknowledged writes across before/after-effect manager crashes, storage latency and automatic container recovery after lease loss")
}

func (h *harness) toxic(method, path string, body any) {
	args := []string{"-n", storeNS, "exec", "toxi-ctl", "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "5", "-X", method, "http://toxiproxy-api:8474" + path}
	if body != nil {
		args = append(args, "-H", "Content-Type: application/json", "-d", encode(body))
	}
	h.k(args...)
}

// crashTarget is observed before the requested contraction. It is independent
// of the controller's persisted target/proof, including after the Pod is gone.
type crashTarget struct {
	Pod, PodUID, IP, Container, Node, Host, HostUID, BootID string
	FleetUID, WorkloadUID                                   string
	Claim                                                   string
	Storage                                                 *claimIdentity
}

func (h *harness) captureCrashTarget(fleetName string, workload object) crashTarget {
	fleet := h.get("celldfleet", fleetName)
	pod := h.get("pod", fleetName+"-2")
	host := str(pod, "spec", "nodeName")
	node := h.cluster("node", host)
	target := crashTarget{
		Pod: nameOf(pod), PodUID: uidOf(pod), IP: str(pod, "status", "podIP"),
		Node: uidOf(pod), Host: host, HostUID: uidOf(node), BootID: str(node, "status", "nodeInfo", "bootID"),
		FleetUID: uidOf(fleet), WorkloadUID: uidOf(workload),
	}
	for _, container := range list(pod, "status", "containerStatuses") {
		if str(container, "name") == "celld" && str(container, "containerID") != "" && str(container, "state", "running", "startedAt") != "" {
			target.Container = fmt.Sprintf("%s/%s/%d", target.PodUID, str(container, "containerID"), num(container, "restartCount"))
		}
	}
	assert(target.Pod == fleetName+"-2" && target.PodUID != "" && target.IP != "" && target.Container != "" && target.Host != "" && target.HostUID != "" && target.BootID != "" && target.FleetUID != "" && target.WorkloadUID != "", "cannot capture exact highest-ordinal crash target: %+v", target)
	if str(fleet, "spec", "profile") == "PersistentFleet" {
		target.Node = target.Pod
		target.Claim = "data-" + target.Pod
		storage, ok := h.claims(fleetName)[target.Claim]
		assert(ok && storage.UID != "" && storage.Volume != "" && storage.VolumeUID != "" && storage.Handle != "", "missing crash target storage identity: %s", target.Claim)
		// claims verifies this CSI driver and returns its raw handle; the
		// controller binds the driver and handle together in its operation.
		storage.Handle = "hostpath.csi.k8s.io:" + storage.Handle
		target.Storage = &storage
	}
	return target
}

// Decode into typed fields so malformed/null targets, nonboolean proofs and
// nonintegral PIDs cannot disappear through the permissive JSON accessors.
func validateCrashProof(raw object, expected crashTarget) error {
	var operation struct {
		ID, Kind, Phase, FleetUID, WorkloadUID string
		From, To                               int
		Targets                                []struct {
			Pod, PodUID, IP, Container, HostUID string
			Identity                            launcher.State
			Storage                             *struct {
				Claim, ClaimUID, ClaimVersion, Volume, VolumeUID, Handle string
				DeletionProtected, CleanupStarted                        bool
			}
			Proof *struct {
				Removal                                           launcher.RemovalResult
				ChildExited, InheritedLockReleased, RestartDenied bool
			}
		}
	}
	if err := json.Unmarshal([]byte(encode(raw)), &operation); err != nil {
		return fmt.Errorf("invalid crash proof encoding: %w", err)
	}
	if operation.ID == "" || operation.Kind != "Scale" || operation.Phase != "Apply" || operation.From != 3 || operation.To != 2 || operation.FleetUID != expected.FleetUID || operation.WorkloadUID != expected.WorkloadUID || len(operation.Targets) != 1 {
		return fmt.Errorf("crash did not retain one exact 3-to-2 operation target")
	}
	target := operation.Targets[0]
	identity := target.Identity
	if target.Pod != expected.Pod || target.PodUID != expected.PodUID || target.IP != expected.IP || target.Container != expected.Container || target.HostUID != expected.HostUID || identity.Node != expected.Node || identity.Host != expected.Host || identity.BootID != expected.BootID || identity.Invocation == "" || identity.Generation == "" || identity.DiskID == "" || identity.PID <= 0 {
		return fmt.Errorf("crash proof does not bind to captured Pod and host incarnation %s", expected.Pod)
	}
	if target.Proof == nil {
		return fmt.Errorf("crash target %s has no strict proof", expected.Pod)
	}
	proof := target.Proof
	removal := proof.Removal
	if removal.Operation != operation.ID || removal.Generation != identity.Generation || removal.Mode != "remove-disk" || removal.Phase != "data_safe" || !removal.ControlOnly || !removal.DataSafe || removal.Blocker != "" || !proof.ChildExited || !proof.InheritedLockReleased || !proof.RestartDenied {
		return fmt.Errorf("crash target %s lacks exact runtime, process, lock or restart proof", expected.Pod)
	}
	if expected.Storage == nil {
		if target.Storage != nil {
			return fmt.Errorf("bucket crash target unexpectedly captured persistent storage")
		}
	} else {
		storage := target.Storage
		if storage == nil || storage.Claim != expected.Claim || storage.ClaimUID != expected.Storage.UID || storage.ClaimVersion == "" || storage.Volume != expected.Storage.Volume || storage.VolumeUID != expected.Storage.VolumeUID || storage.Handle != expected.Storage.Handle || !storage.DeletionProtected || storage.CleanupStarted {
			return fmt.Errorf("crash target %s does not retain exact protected disk identity", expected.Pod)
		}
	}
	return nil
}
