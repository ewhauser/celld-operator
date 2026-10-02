package controller

import (
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	switch w := want.(type) {
	case *appsv1.Deployment:
		desired, actual := w.Spec.DeepCopy(), got.(*appsv1.Deployment).Spec.DeepCopy()
		for _, o := range []*appsv1.DeploymentSpec{desired, actual} {
			if o.RevisionHistoryLimit == nil {
				o.RevisionHistoryLimit = new(int32(10))
			}
			if o.ProgressDeadlineSeconds == nil {
				o.ProgressDeadlineSeconds = new(int32(600))
			}
			normalizePod(&o.Template.Spec)
		}
		return equality.Semantic.DeepEqual(*desired, *actual)
	case *appsv1.StatefulSet:
		desired, actual := w.Spec.DeepCopy(), got.(*appsv1.StatefulSet).Spec.DeepCopy()
		for _, o := range []*appsv1.StatefulSetSpec{desired, actual} {
			if o.RevisionHistoryLimit == nil {
				o.RevisionHistoryLimit = new(int32(10))
			}
			if o.Ordinals != nil && o.Ordinals.Start == 0 {
				o.Ordinals = nil
			}
			// MaxUnavailableStatefulSet clusters default the rolling bound to one,
			// which is also the behavior without that feature gate.
			if u := o.UpdateStrategy.RollingUpdate; u != nil && u.MaxUnavailable != nil && *u.MaxUnavailable == intstr.FromInt32(1) {
				u.MaxUnavailable = nil
			}
			normalizePod(&o.Template.Spec)
			for i := range o.VolumeClaimTemplates {
				claim := &o.VolumeClaimTemplates[i]
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
		return equality.Semantic.DeepEqual(*desired, *actual)
	case *corev1.Service:
		g := got.(*corev1.Service)
		// Allocation is expected for ClusterIP Services, but conversion to/from
		// headless routing is a configuration change, not an allocated address.
		if (w.Spec.ClusterIP == corev1.ClusterIPNone) != (g.Spec.ClusterIP == corev1.ClusterIPNone) {
			return false
		}
		// Only port values are mutated below; other nested spec fields are read-only.
		desired, actual := w.Spec, g.Spec
		desired.Ports, actual.Ports = slices.Clone(desired.Ports), slices.Clone(actual.Ports)
		for _, o := range []*corev1.ServiceSpec{&desired, &actual} {
			o.ClusterIP = ""
			o.ClusterIPs = nil
			o.IPFamilies = nil
			if o.IPFamilyPolicy == nil {
				o.IPFamilyPolicy = new(corev1.IPFamilyPolicySingleStack)
			}
			if o.InternalTrafficPolicy == nil {
				o.InternalTrafficPolicy = new(corev1.ServiceInternalTrafficPolicyCluster)
			}
			if o.SessionAffinity == "" {
				o.SessionAffinity = corev1.ServiceAffinityNone
			}
			for i := range o.Ports {
				if o.Ports[i].Protocol == "" {
					o.Ports[i].Protocol = corev1.ProtocolTCP
				}
			}
		}
		return equality.Semantic.DeepEqual(desired, actual)
	case *networkingv1.NetworkPolicy:
		return equality.Semantic.DeepEqual(&w.Spec, &got.(*networkingv1.NetworkPolicy).Spec)
	case *policyv1.PodDisruptionBudget:
		return equality.Semantic.DeepEqual(&w.Spec, &got.(*policyv1.PodDisruptionBudget).Spec)
	default:
		return false
	}
}

// introducedFields lists operator-owned prerequisite fields that a release added
// after earlier releases had already created the object. Each entry copies one
// field from want into got only where got still holds the field's API zero
// value; it never overwrites a value, so anything set by another writer stays a
// conflict. Entries are keyed by field, not by release, and apply to every
// matching object; retire one only when no supported upgrade path can create
// the object without it.
var introducedFields = []func(want, got client.Object){
	// Declared ServicePort.appProtocol (#59). Ports are paired by position and
	// name; a renamed, added or reordered port leaves the conflict in place.
	func(want, got client.Object) {
		w, ok := want.(*corev1.Service)
		g, gok := got.(*corev1.Service)
		if !ok || !gok || len(w.Spec.Ports) != len(g.Spec.Ports) {
			return
		}
		for i := range g.Spec.Ports {
			if g.Spec.Ports[i].AppProtocol == nil && g.Spec.Ports[i].Name == w.Spec.Ports[i].Name && w.Spec.Ports[i].AppProtocol != nil {
				g.Spec.Ports[i].AppProtocol = new(*w.Spec.Ports[i].AppProtocol)
			}
		}
	},
}

// backfill returns got with only introducedFields filled from want, or nil when
// that is not enough to make it match. A nil result is a conflict: the object
// differs in something other than a field this release newly declares.
func backfill(want, got client.Object) client.Object {
	upgraded := got.DeepCopyObject().(client.Object)
	for _, fill := range introducedFields {
		fill(want, upgraded)
	}
	if !matches(want, upgraded) {
		return nil
	}
	return upgraded
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
			for _, probe := range []*corev1.Probe{c.ReadinessProbe, c.LivenessProbe, c.StartupProbe} {
				if probe != nil && probe.HTTPGet != nil && probe.HTTPGet.Scheme == "" {
					probe.HTTPGet.Scheme = corev1.URISchemeHTTP
				}
			}
		}
	}
}
