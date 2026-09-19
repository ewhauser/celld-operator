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
//     operator's journaled addition path.
//  3. A lowered HPA maximum contracts the fleet through the same gated executor
//     the disposable fixture uses for automatic contraction; the acknowledged
//     write stays readable.
//  4. Deleting the HPA and switching the policy off returns ownership to the user.
func (h *harness) exerciseExternal() {
	scale := func() object {
		return decode(h.k("get", "--raw", "/apis/celld.eric.dev/v1alpha1/namespaces/fleets/celldfleets/alpha/scale"))
	}
	settled := func(count int64) bool {
		return specReplicas(h.get("deployment", "alpha")) == count && h.ready("alpha") && len(sub(h.journal("alpha"), "Operation")) == 0
	}
	h.merge("alpha", `{"spec":{"capacity":{"mode":"External"}}}`)
	h.wait("External mode names the /scale writer as owner", func() bool {
		return str(h.get("celldfleet", "alpha"), "status", "capacity", "reason") == "ExternalOwner"
	})
	view := scale()
	assert(specReplicas(view) == 2 && num(view, "status", "replicas") == 2 && strings.HasPrefix(str(view, "status", "selector"), "celld.eric.dev/fleet-uid="), "%v", view)
	fmt.Println("PASS: /scale reports spec, observed replicas and the fleet selector")

	// Three nodes with strict hostname separation bound the fleet at three replicas.
	h.apply(&autoscalingv2.HorizontalPodAutoscaler{
		APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler",
		Name: "alpha", Namespace: "fleets",
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "celld.eric.dev/v1alpha1", Kind: "CelldFleet", Name: "alpha"},
			MinReplicas:    new(int32(2)),
			MaxReplicas:    3,
			// Any CPU use exceeds 1% of the 250m request, so the HPA drives to max.
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
	h.waitFor("HPA raises spec.replicas to its maximum", 300*time.Second, func() bool {
		return specReplicas(h.get("celldfleet", "alpha")) == 3
	})
	h.waitFor("operator applies the HPA addition through the journal", 300*time.Second, func() bool { return settled(3) })
	gen := generation(h.get("celldfleet", "alpha"))
	h.sleep(20 * time.Second)
	assert(generation(h.get("celldfleet", "alpha")) == gen, "operator or HPA kept rewriting spec.replicas")
	fmt.Println("PASS: External mode never fights the HPA; desired 3 applied 3")

	// Lower the ceiling: the HPA requests contraction; the fixture executor runs it.
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
	h.waitFor("kubectl scale through /scale adds a replica via the journal", 300*time.Second, func() bool { return settled(3) })

	// Return ownership: drop the policy; spec.replicas is a manual field again.
	h.merge("alpha", `{"spec":{"capacity":null,"replicas":2}}`)
	h.waitFor("manual ownership restored after External mode", 600*time.Second, func() bool { return settled(2) })
	fmt.Println(h.k("get", "celldstoragereservations", "-o", "json"))
}
