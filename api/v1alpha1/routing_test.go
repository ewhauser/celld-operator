package v1alpha1

import "testing"

func routingFixture() *RoutingSpec {
	return &RoutingSpec{Hostnames: []RouteHostname{"app.example.com", "*.example.net"}, Source: RoutingSource{Namespace: "edge", PodLabels: map[string]string{"app": "gateway"}}, Gateway: &GatewayRouting{Name: "public", SectionName: "https"}}
}
func TestRoutingValidation(t *testing.T) {
	f := valid()
	f.Spec.Routing = routingFixture()
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*RoutingSpec){
		"neither":         func(r *RoutingSpec) { r.Gateway = nil },
		"both":            func(r *RoutingSpec) { r.Ingress = &IngressRouting{ClassName: "nginx"} },
		"no hosts":        func(r *RoutingSpec) { r.Hostnames = nil },
		"catchall":        func(r *RoutingSpec) { r.Hostnames = []RouteHostname{"*"} },
		"IP":              func(r *RoutingSpec) { r.Hostnames = []RouteHostname{"127.0.0.1"} },
		"duplicate":       func(r *RoutingSpec) { r.Hostnames = append(r.Hostnames, r.Hostnames[0]) },
		"empty selector":  func(r *RoutingSpec) { r.Source.PodLabels = nil },
		"empty namespace": func(r *RoutingSpec) { r.Source.Namespace = "" },
		"bad label":       func(r *RoutingSpec) { r.Source.PodLabels["app"] = "not a label" },
		"bad gateway":     func(r *RoutingSpec) { r.Gateway.Name = "" },
		"bad listener":    func(r *RoutingSpec) { r.Gateway.SectionName = "has spaces" },
		"no class":        func(r *RoutingSpec) { r.Gateway = nil; r.Ingress = &IngressRouting{} },
		"reserved annotation": func(r *RoutingSpec) {
			r.Gateway = nil
			r.Ingress = &IngressRouting{ClassName: "nginx", Annotations: map[string]string{"celld.eric.dev/routing-annotation-keys": "[]"}}
		},
		"legacy class": func(r *RoutingSpec) {
			r.Gateway = nil
			r.Ingress = &IngressRouting{ClassName: "nginx", Annotations: map[string]string{"kubernetes.io/ingress.class": "other"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := routingFixture()
			change(r)
			if validateRouting(r) == nil {
				t.Fatal("accepted invalid routing")
			}
		})
	}
	f.Spec.Routing.Gateway = nil
	f.Spec.Routing.Ingress = &IngressRouting{ClassName: "nginx", TLSSecretName: "app-tls", Annotations: map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt"}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}
