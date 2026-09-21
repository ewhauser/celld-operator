package main

import "testing"

func crashProofFixture() (object, crashTarget) {
	raw := decode(`{
		"ID":"operation-1", "Kind":"Scale", "Phase":"Apply", "From":3, "To":2,
		"FleetUID":"fleet-uid", "WorkloadUID":"workload-uid",
		"Targets":[{
			"Pod":"beta-2", "PodUID":"pod-uid", "IP":"10.0.0.12", "Container":"pod-uid/containerd://child/0", "HostUID":"host-uid",
			"Identity":{"Node":"beta-2", "Host":"worker-1", "BootID":"boot-1", "Invocation":"invocation-1", "Generation":"generation-1", "DiskID":"disk-1", "PID":22},
			"Storage":{"Claim":"data-beta-2", "ClaimUID":"claim-uid", "ClaimVersion":"123", "Volume":"pv-1", "VolumeUID":"pv-uid", "Handle":"hostpath.csi.k8s.io:csi-1", "DeletionProtected":true},
			"Proof":{"Removal":{"Operation":"operation-1", "Generation":"generation-1", "Mode":"remove-disk", "Phase":"data_safe", "ControlOnly":true, "DataSafe":true, "Blocker":""}, "ChildExited":true, "InheritedLockReleased":true, "RestartDenied":true}
		}]
	}`)
	return raw, crashTarget{
		Pod: "beta-2", PodUID: "pod-uid", IP: "10.0.0.12", Container: "pod-uid/containerd://child/0",
		Node: "beta-2", Host: "worker-1", HostUID: "host-uid", BootID: "boot-1",
		FleetUID: "fleet-uid", WorkloadUID: "workload-uid", Claim: "data-beta-2",
		Storage: &claimIdentity{UID: "claim-uid", Volume: "pv-1", VolumeUID: "pv-uid", Handle: "hostpath.csi.k8s.io:csi-1"},
	}
}

func TestCrashProofRequiresExactTargetAndEveryBarrier(t *testing.T) {
	valid, expected := crashProofFixture()
	if err := validateCrashProof(valid, expected); err != nil {
		t.Fatal(err)
	}
	target := func(op object) object { return op["Targets"].([]any)[0].(object) }
	proof := func(op object) object { return target(op)["Proof"].(object) }
	removal := func(op object) object { return proof(op)["Removal"].(object) }
	identity := func(op object) object { return target(op)["Identity"].(object) }
	storage := func(op object) object { return target(op)["Storage"].(object) }
	cases := map[string]func(object){
		"missing targets": func(op object) { delete(op, "Targets") },
		"empty targets":   func(op object) { op["Targets"] = []any{} },
		"empty target":    func(op object) { op["Targets"] = []any{object{}} },
		"null target":     func(op object) { op["Targets"] = []any{nil} },
		"extra target":    func(op object) { op["Targets"] = append(op["Targets"].([]any), target(op)) },
		"extra null target": func(op object) {
			op["Targets"] = append(op["Targets"].([]any), nil)
		},
		"foreign ordinal":      func(op object) { target(op)["Pod"] = "beta-1" },
		"foreign Pod UID":      func(op object) { target(op)["PodUID"] = "replacement" },
		"foreign address":      func(op object) { target(op)["IP"] = "10.0.0.99" },
		"restarted container":  func(op object) { target(op)["Container"] = "pod-uid/containerd://child/1" },
		"foreign host UID":     func(op object) { target(op)["HostUID"] = "replacement" },
		"foreign runtime node": func(op object) { identity(op)["Node"] = "beta-1" },
		"foreign host":         func(op object) { identity(op)["Host"] = "worker-2" },
		"rebooted host":        func(op object) { identity(op)["BootID"] = "boot-2" },
		"zero PID":             func(op object) { identity(op)["PID"] = 0 },
		"fractional PID":       func(op object) { identity(op)["PID"] = 22.5 },
		"missing proof":        func(op object) { delete(target(op), "Proof") },
		"empty proof":          func(op object) { target(op)["Proof"] = object{} },
		"empty removal":        func(op object) { proof(op)["Removal"] = object{} },
		"foreign operation":    func(op object) { removal(op)["Operation"] = "operation-2" },
		"foreign generation":   func(op object) { removal(op)["Generation"] = "generation-2" },
		"ordinary shutdown":    func(op object) { removal(op)["Mode"] = "preserve" },
		"acceptance only":      func(op object) { removal(op)["Phase"] = "draining" },
		"failed runtime":       func(op object) { removal(op)["Phase"] = "failed" },
		"runtime blocker":      func(op object) { removal(op)["Blocker"] = "proof pending" },
		"nonboolean proof":     func(op object) { proof(op)["ChildExited"] = "true" },
		"missing storage":      func(op object) { delete(target(op), "Storage") },
		"foreign claim":        func(op object) { storage(op)["Claim"] = "data-beta-1" },
		"foreign claim UID":    func(op object) { storage(op)["ClaimUID"] = "replacement" },
		"foreign volume":       func(op object) { storage(op)["Volume"] = "pv-2" },
		"foreign volume UID":   func(op object) { storage(op)["VolumeUID"] = "replacement" },
		"foreign CSI handle":   func(op object) { storage(op)["Handle"] = "csi-2" },
		"no claim version":     func(op object) { delete(storage(op), "ClaimVersion") },
		"no backend protection": func(op object) {
			storage(op)["DeletionProtected"] = false
		},
		"premature cleanup": func(op object) { storage(op)["CleanupStarted"] = true },
	}
	for _, key := range []string{"Invocation", "Generation", "DiskID"} {
		cases["missing process "+key] = func(op object) { delete(identity(op), key) }
	}
	for _, key := range []string{"ChildExited", "InheritedLockReleased", "RestartDenied"} {
		cases["missing "+key] = func(op object) { delete(proof(op), key) }
	}
	for _, key := range []string{"ControlOnly", "DataSafe"} {
		cases["missing "+key] = func(op object) { delete(removal(op), key) }
	}
	for _, key := range []string{"ID", "Kind", "Phase", "FleetUID", "WorkloadUID", "From", "To"} {
		cases["missing operation "+key] = func(op object) { delete(op, key) }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			op, captured := crashProofFixture()
			mutate(op)
			if err := validateCrashProof(op, captured); err == nil {
				t.Fatalf("incomplete or foreign crash proof accepted: %v", op)
			}
		})
	}
}

func TestBucketCrashProofUsesPodUIDAndNoPersistentStorage(t *testing.T) {
	op, expected := crashProofFixture()
	expected.Node = expected.PodUID
	expected.Claim, expected.Storage = "", nil
	target := op["Targets"].([]any)[0].(object)
	target["Identity"].(object)["Node"] = expected.PodUID
	if err := validateCrashProof(op, expected); err == nil {
		t.Fatal("Bucket target accepted persistent storage proof")
	}
	delete(target, "Storage")
	if err := validateCrashProof(op, expected); err != nil {
		t.Fatal(err)
	}
}
