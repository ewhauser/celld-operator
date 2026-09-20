package main

import (
	"fmt"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// exerciseExternal proves External capacity mode against a real HPA controller
// and Metrics Server. It runs only after the base fixtures pass.
//
//  1. The /scale subresource reports the fleet's pods and selector, and a write
//     through it lands in spec.replicas without the operator ever writing it back.
//  2. An HPA targeting the CelldFleet raises the fleet to its maximum through the
//     operator's bounded addition path.
//  3. A lowered HPA maximum contracts the fleet through the same gated executor
//     the disposable fixture uses for automatic contraction; the acknowledged
//     write stays readable.
//  4. Deleting the HPA and switching the policy off returns ownership to the user.
func (h *harness) exerciseExternal() {
	scale := func() object {
		return decode(h.k("get", "--raw", "/apis/celld.eric.dev/v1alpha1/namespaces/fleets/celldfleets/alpha/scale"))
	}
	settled := func(count int64) bool {
		return specReplicas(h.get("statefulset", "alpha")) == count && h.ready("alpha") && len(sub(h.currentState("alpha"), "Operation")) == 0
	}
	h.merge("alpha", `{"spec":{"capacity":{"mode":"External"}}}`)
	h.wait("External mode names the /scale writer as owner", func() bool {
		return str(h.get("celldfleet", "alpha"), "status", "capacity", "reason") == "ExternalOwner"
	})
	view := scale()
	assert(specReplicas(view) == 2 && num(view, "status", "replicas") == 2 && strings.HasPrefix(str(view, "status", "selector"), "celld.eric.dev/fleet-uid="), "%v", view)
	fmt.Println("PASS: /scale reports spec, observed replicas and the fleet selector")

	// The HPA only acts on load it can measure, and it measures CPU as an integer
	// percentage of the pods' requests. Against the 250m request a 1% target needs
	// the fleet to reach 2% -- 10 millicores across two replicas -- before any
	// raise is computed at all, because the controller's 10% tolerance holds it
	// still at exactly 1%. An idle celld fleet stays under that, so this scenario
	// drives real application traffic for as long as the HPA owns the count. The
	// generator is a client of the fleet, never a member: it carries the client-of
	// label the NetworkPolicy admits and no fleet-uid label, so it is not part of
	// the Pod set the HPA measures or the operator counts.
	h.startFleetLoad("alpha")
	// Three nodes with strict hostname separation bound the fleet at three replicas.
	h.apply(&autoscalingv2.HorizontalPodAutoscaler{
		APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler",
		Name: "alpha", Namespace: "fleets",
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "celld.eric.dev/v1alpha1", Kind: "CelldFleet", Name: "alpha"},
			MinReplicas:    new(int32(2)),
			MaxReplicas:    3,
			// Two percent of the 250m request is the first utilization the
			// controller acts on; the load generator keeps the fleet above it.
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   "cpu",
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: new(int32(1))},
				},
			}},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: new(int32(0)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 15}},
				},
				ScaleUp: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: new(int32(0))},
			},
		},
	})
	h.waitFor("HPA reads the fleet through /scale", 240*time.Second, func() bool {
		return num(h.get("hpa", "alpha"), "status", "currentReplicas") >= 2
	})
	// Separating this from the raise below names the failure: no measurable load
	// is a Metrics Server or fixture problem, not the operator refusing a count.
	h.waitFor("HPA measures the fleet above its target through Metrics Server", 300*time.Second, func() bool {
		return hpaCPUUtilization(h.get("hpa", "alpha")) >= 2
	})
	h.waitFor("HPA raises spec.replicas to its maximum", 300*time.Second, func() bool {
		return specReplicas(h.get("celldfleet", "alpha")) == 3
	})
	h.waitFor("operator applies the HPA addition through current-operation state", 300*time.Second, func() bool { return settled(3) })
	gen := generation(h.get("celldfleet", "alpha"))
	h.sleep(20 * time.Second)
	assert(generation(h.get("celldfleet", "alpha")) == gen, "operator or HPA kept rewriting spec.replicas")
	fmt.Println("PASS: External mode never fights the HPA; desired 3 applied 3")

	// Lower the ceiling: the HPA requests contraction; the fixture executor runs
	// it. That executor demands fresh low-demand survivor evidence, so the load
	// generator stops before the ceiling moves.
	h.stopFleetLoad()
	h.k("-n", "fleets", "patch", "hpa", "alpha", "--type=merge", "-p", `{"spec":{"minReplicas":2,"maxReplicas":2}}`)
	h.waitFor("HPA lowers spec.replicas", 300*time.Second, func() bool {
		return specReplicas(h.get("celldfleet", "alpha")) == 2
	})
	h.waitFor("HPA-requested contraction executes through the gated Bucket executor (local fixture)", 600*time.Second, func() bool { return settled(2) })
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable after HPA-driven shrink")
	view = scale()
	assert(specReplicas(view) == 2 && num(view, "status", "replicas") == 2, "%v", view)
	fmt.Println("PASS: acknowledged write readable after HPA-driven shrink; /scale consistent")

	// kubectl scale is an ordinary /scale writer once the HPA is gone.
	h.k("-n", "fleets", "delete", "hpa", "alpha", "--wait=true")
	h.k("-n", "fleets", "scale", "celldfleet/alpha", "--replicas=3")
	h.waitFor("kubectl scale through /scale adds a replica via current-operation state", 300*time.Second, func() bool { return settled(3) })

	// Return ownership: drop the policy; spec.replicas is a manual field again.
	h.merge("alpha", `{"spec":{"capacity":null,"replicas":2}}`)
	h.waitFor("manual ownership restored after External mode", 600*time.Second, func() bool { return settled(2) })
	fmt.Println(h.k("get", "celldstoragereservations", "-o", "json"))
}

// hpaCPUUtilization reports the CPU utilization percentage the HPA last measured,
// or -1 while it has published none.
func hpaCPUUtilization(hpa object) int64 {
	for _, metric := range list(hpa, "status", "currentMetrics") {
		if str(metric, "type") == "Resource" && str(metric, "resource", "name") == "cpu" {
			return num(metric, "resource", "current", "averageUtilization")
		}
	}
	return -1
}

// startFleetLoad runs one client Pod that issues application requests at a fixed
// rate, so the fleet's CPU use is a property of the fixture rather than of the
// machine it runs on. curl keeps a single connection for the whole URL range and
// --rate paces the requests, which keeps the fleet measurably busy without
// saturating a shared CI runner.
func (h *harness) startFleetLoad(fleetName string) {
	pod := sleepPod("load", "fleets", map[string]string{"celld.eric.dev/client-of": fleetName})
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", fmt.Sprintf(
		`while true; do curl -s -o /dev/null --rate 100/s "http://%s:8080/?cell=integration&id=ack&n=[1-100000]" || true; done`, fleetName)}
	h.apply(pod)
	h.waitFor("fleet load generator running", 120*time.Second, func() bool {
		return str(h.getIn("fleets", "pod", "load"), "status", "phase") == "Running"
	})
}

func (h *harness) stopFleetLoad() {
	h.k("-n", "fleets", "delete", "pod", "load", "--wait=true")
}
