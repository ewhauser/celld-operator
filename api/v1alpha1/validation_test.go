package v1alpha1

import (
	"testing"
)

func valid() *CelldFleet {
	f := &CelldFleet{
		Name: "test",
		Spec: CelldFleetSpec{
			Qualification:      "Experimental",
			Profile:            "Bucket",
			ServiceAccountName: "runtime",
			Storage:            StorageSpec{Bucket: "example-bucket", Region: "us-east-1"},
			Placement:          PlacementSpec{AZCount: 2, Zones: []string{"us-east-1a", "us-east-1b"}},
		},
	}
	f.Default()
	return f
}
func TestDefaultsAndValidation(t *testing.T) {
	f := valid()
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.Spec.Replicas != 3 || f.Spec.Storage.SizeGiB != 10 || f.Spec.Placement.Mode != "Strict" {
		t.Fatal("incorrect defaults")
	}
	tests := map[string]func(*CelldFleet){
		"production":      func(f *CelldFleet) { f.Spec.Qualification = "Production" },
		"prefix":          func(f *CelldFleet) { f.Spec.Storage.Bucket = "example-bucket/shared" },
		"alias":           func(f *CelldFleet) { f.Spec.Storage.Bucket = "EXAMPLE-bucket" },
		"profile":         func(f *CelldFleet) { f.Spec.Profile = "Unknown" },
		"storage class":   func(f *CelldFleet) { f.Spec.Profile = "PersistentFleet" },
		"bucket pvc":      func(f *CelldFleet) { f.Spec.Storage.StorageClassName = "ebs" },
		"replica floor":   func(f *CelldFleet) { f.Spec.Replicas = 1 },
		"az count":        func(f *CelldFleet) { f.Spec.Placement.AZCount = 3 },
		"duplicate zones": func(f *CelldFleet) { f.Spec.Placement.Zones[1] = "us-east-1a" },
		"foreign region":  func(f *CelldFleet) { f.Spec.Placement.Zones[1] = "us-west-2a" },
		"sa":              func(f *CelldFleet) { f.Spec.ServiceAccountName = "" },
		"name":            func(f *CelldFleet) { f.Name = "has.dot" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := valid()
			mutate(f)
			if f.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
