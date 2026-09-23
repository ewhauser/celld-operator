package v1alpha1

import "testing"

func TestSharedStorageValidation(t *testing.T) {
	validStorage := StorageSpec{Bucket: "shared-previews", Region: "us-east-1", SizeGiB: 10, Prefix: "preview-one", PreviewFleetRef: &StorageFleetReference{Name: "pool", UID: "uid"}, Scratch: &ScratchSpec{Request: "64Mi", Limit: "512Mi"}, Endpoint: &ObjectStoreEndpoint{URL: "http://store.fleets.svc:9000", CredentialsSecretName: "store", Egress: CollectorEgress{PodLabels: map[string]string{"app": "store"}}}}
	if err := validateStorageOptions(validStorage, "Bucket"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*StorageSpec){
		"missing pool":                  func(s *StorageSpec) { s.PreviewFleetRef = nil },
		"missing prefix":                func(s *StorageSpec) { s.Prefix = "" },
		"nested prefix":                 func(s *StorageSpec) { s.Prefix = "one/two" },
		"escaped prefix":                func(s *StorageSpec) { s.Prefix = "one%2ftwo" },
		"empty pool identity":           func(s *StorageSpec) { s.PreviewFleetRef.UID = "" },
		"zero scratch":                  func(s *StorageSpec) { s.Scratch.Request = "0" },
		"scratch request exceeds limit": func(s *StorageSpec) { s.Scratch.Request = "1Gi" },
		"invalid scratch":               func(s *StorageSpec) { s.Scratch.Limit = "bogus" },
		"missing credentials":           func(s *StorageSpec) { s.Endpoint.CredentialsSecretName = "" },
		"missing egress":                func(s *StorageSpec) { s.Endpoint.Egress = CollectorEgress{} },
		"path":                          func(s *StorageSpec) { s.Endpoint.URL = "http://store/prefix" },
		"query":                         func(s *StorageSpec) { s.Endpoint.URL = "http://store?bucket=other" },
		"fragment":                      func(s *StorageSpec) { s.Endpoint.URL = "http://store#fragment" },
		"userinfo":                      func(s *StorageSpec) { s.Endpoint.URL = "http://user:pass@store" },
		"escaped slash":                 func(s *StorageSpec) { s.Endpoint.URL = "http://store/%2f" },
		"bad port":                      func(s *StorageSpec) { s.Endpoint.URL = "http://store:70000" },
	} {
		t.Run(name, func(t *testing.T) {
			s := validStorage.DeepCopy()
			mutate(s)
			if validateStorageOptions(*s, "Bucket") == nil {
				t.Fatal("invalid shared storage accepted")
			}
		})
	}
	if validateStorageOptions(validStorage, "PersistentFleet") == nil {
		t.Fatal("shared persistent storage accepted")
	}
	if got := validStorage.URL(); got != "s3://shared-previews/preview-one" {
		t.Fatal(got)
	}
	if got := (StorageSpec{Bucket: "dedicated"}).URL(); got != "s3://dedicated" {
		t.Fatal(got)
	}
}
