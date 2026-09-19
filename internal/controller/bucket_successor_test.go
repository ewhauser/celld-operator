package controller

import (
	"testing"
	"time"

	"github.com/ewhauser/celld-operator/internal/capacity"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBucketAdmittedContainerRestartSuccessor(t *testing.T) {
	for _, name := range []string{"qualified successor", "unknown predecessor", "same container", "revived predecessor", "successor missing"} {
		t.Run(name, func(t *testing.T) {
			p, f, j, opts, reader := bucketPreflightSetup(t)
			r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
			collect := func() {
				pods := &corev1.PodList{}
				if err := p.client.List(t.Context(), pods); err != nil {
					t.Fatal(err)
				}
				o := capacity.Observation{At: reader.now, Complete: true}
				for i := range pods.Items {
					id, _ := podIdentity(&pods.Items[i])
					o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: reader.now, RuntimeReceived: reader.now, MetricsAt: reader.now, MetricsReceived: reader.now, Window: 15 * time.Second})
				}
				r.Collector = bucketCapacityCollector{o}
			}
			collect()
			admitted, _, err := r.bucketAssessment(t.Context(), f, j, 3, false)
			if err != nil {
				t.Fatal(err)
			}
			j.Operation.BucketCandidates = admitted
			if name == "unknown predecessor" {
				j.Operation.BucketCandidates = nil
			}
			pod := &corev1.Pod{}
			if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-1"}, pod); err != nil {
				t.Fatal(err)
			}
			if name != "same container" {
				pod.Status.ContainerStatuses[0].ContainerID = "successor-container"
				pod.Status.ContainerStatuses[0].RestartCount++
			}
			if err := p.client.Status().Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			id, _ := podIdentity(pod)
			old := j.Inventory.Sessions[1]
			j.Inventory.Sessions[1].Current = false
			current := old
			current.Container = id
			current.Generation = "successor"
			current.Current = true
			j.Inventory.Sessions = append(j.Inventory.Sessions, current)
			reader.generation = map[string]string{"pod-1": "successor"}
			collect()
			sessions, _, err := r.bucketAssessment(t.Context(), f, j, 3, false)
			if name == "unknown predecessor" || name == "same container" {
				if err == nil {
					t.Fatal("unqualified succession admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, s := range sessions {
				if s.Node == "pod-1" && s.Generation == "generation" {
					found = s.Retired && s.SupersededBy == "successor" && !s.ExpiryObserved
				}
			}
			if !found {
				t.Fatal("exact predecessor supersession not retained")
			}
			j.Operation.BucketCandidates = sessions
			switch name {
			case "revived predecessor":
				reader.generation = nil
			case "successor missing":
				reader.nodes = []string{"pod-0", "pod-2"}
			}
			_, _, err = r.bucketAssessment(t.Context(), f, j, 3, false)
			if (name == "qualified successor") != (err == nil) {
				t.Fatalf("contrary evidence acceptance: %v", err)
			}
		})
	}
}
