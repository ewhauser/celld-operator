package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// matches compares complete controlled specs, including additional containers,
// environment entries, probes, selectors and policy rules. Only explicit API
// defaults and server-allocated Service addresses are normalized; unknown
// defaulting/admission changes fail closed instead of becoming wildcards.
func matches(want, got client.Object) bool {
	if got.GetLabels()[FleetLabel] != want.GetLabels()[FleetLabel] || len(got.GetOwnerReferences()) != 0 || !got.GetDeletionTimestamp().IsZero() {
		return false
	}
	want = want.DeepCopyObject().(client.Object)
	got = got.DeepCopyObject().(client.Object)
	switch w := want.(type) {
	case *appsv1.Deployment:
		g := got.(*appsv1.Deployment)
		for _, o := range []*appsv1.Deployment{w, g} {
			if o.Spec.RevisionHistoryLimit == nil {
				o.Spec.RevisionHistoryLimit = new(int32(10))
			}
			if o.Spec.ProgressDeadlineSeconds == nil {
				o.Spec.ProgressDeadlineSeconds = new(int32(600))
			}
			normalizePod(&o.Spec.Template.Spec)
		}
		return equality.Semantic.DeepEqual(w.Spec, g.Spec)
	case *appsv1.StatefulSet:
		g := got.(*appsv1.StatefulSet)
		for _, o := range []*appsv1.StatefulSet{w, g} {
			if o.Spec.RevisionHistoryLimit == nil {
				o.Spec.RevisionHistoryLimit = new(int32(10))
			}
			if o.Spec.Ordinals != nil && o.Spec.Ordinals.Start == 0 {
				o.Spec.Ordinals = nil
			}
			normalizePod(&o.Spec.Template.Spec)
			for i := range o.Spec.VolumeClaimTemplates {
				claim := &o.Spec.VolumeClaimTemplates[i]
				if claim.Kind == "" {
					claim.Kind = "PersistentVolumeClaim"
				}
				if claim.APIVersion == "" {
					claim.APIVersion = "v1"
				}
				if claim.Spec.VolumeMode == nil {
					claim.Spec.VolumeMode = new(corev1.PersistentVolumeFilesystem)
				}
				if claim.Status.Phase == "" {
					claim.Status.Phase = corev1.ClaimPending
				}
			}
		}
		return equality.Semantic.DeepEqual(w.Spec, g.Spec)
	case *corev1.Service:
		g := got.(*corev1.Service)
		// Allocation is expected for ClusterIP Services, but conversion to/from
		// headless routing is a configuration change, not an allocated address.
		if (w.Spec.ClusterIP == corev1.ClusterIPNone) != (g.Spec.ClusterIP == corev1.ClusterIPNone) {
			return false
		}
		for _, o := range []*corev1.Service{w, g} {
			o.Spec.ClusterIP = ""
			o.Spec.ClusterIPs = nil
			o.Spec.IPFamilies = nil
			if o.Spec.IPFamilyPolicy == nil {
				o.Spec.IPFamilyPolicy = new(corev1.IPFamilyPolicySingleStack)
			}
			if o.Spec.InternalTrafficPolicy == nil {
				o.Spec.InternalTrafficPolicy = new(corev1.ServiceInternalTrafficPolicyCluster)
			}
			if o.Spec.SessionAffinity == "" {
				o.Spec.SessionAffinity = corev1.ServiceAffinityNone
			}
			for i := range o.Spec.Ports {
				if o.Spec.Ports[i].Protocol == "" {
					o.Spec.Ports[i].Protocol = corev1.ProtocolTCP
				}
			}
		}
		return equality.Semantic.DeepEqual(w.Spec, g.Spec)
	case *networkingv1.NetworkPolicy:
		return equality.Semantic.DeepEqual(w.Spec, got.(*networkingv1.NetworkPolicy).Spec)
	case *policyv1.PodDisruptionBudget:
		return equality.Semantic.DeepEqual(w.Spec, got.(*policyv1.PodDisruptionBudget).Spec)
	default:
		return false
	}
}

func normalizePod(p *corev1.PodSpec) {
	if p.RestartPolicy == "" {
		p.RestartPolicy = corev1.RestartPolicyAlways
	}
	if p.DNSPolicy == "" {
		p.DNSPolicy = corev1.DNSClusterFirst
	}
	if p.SchedulerName == "" {
		p.SchedulerName = corev1.DefaultSchedulerName
	}
	if p.DeprecatedServiceAccount == "" {
		p.DeprecatedServiceAccount = p.ServiceAccountName
	}
	for _, containers := range [][]corev1.Container{p.Containers, p.InitContainers} {
		for i := range containers {
			c := &containers[i]
			if c.TerminationMessagePath == "" {
				c.TerminationMessagePath = corev1.TerminationMessagePathDefault
			}
			if c.TerminationMessagePolicy == "" {
				c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
			}
			for j := range c.Ports {
				if c.Ports[j].Protocol == "" {
					c.Ports[j].Protocol = corev1.ProtocolTCP
				}
			}
			for j := range c.Env {
				e := &c.Env[j]
				if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.APIVersion == "" {
					e.ValueFrom.FieldRef.APIVersion = "v1"
				}
			}
			if c.ReadinessProbe != nil && c.ReadinessProbe.HTTPGet != nil && c.ReadinessProbe.HTTPGet.Scheme == "" {
				c.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTP
			}
		}
	}
}
