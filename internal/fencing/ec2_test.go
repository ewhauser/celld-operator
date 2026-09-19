package fencing

import "testing"

func TestExactInstanceAndRetainedDiskRequired(t *testing.T) {
	b := Binding{Account: "123456789012", Region: "us-east-1", Zone: "us-east-1a", Instance: "i-0123456789abcdef0", Volume: "vol-0123456789abcdef0", FleetUID: "fleet", HostUID: "host", BootID: "boot"}
	for _, fault := range []string{"valid", "account", "instance", "zone", "fleet", "host", "boot", "optout", "delete", "missing", "otherdisk", "ambiguousdisk", "absent", "terminated-without-intent", "terminated-with-intent"} {
		t.Run(fault, func(t *testing.T) {
			i := Instance{Account: b.Account, ID: b.Instance, Zone: b.Zone, State: "running", Tags: map[string]string{FleetTag: b.FleetUID, HostTag: b.HostUID, BootTag: b.BootID, FenceTag: "terminate"}, Disks: []Disk{{ID: b.Volume}}}
			switch fault {
			case "account":
				i.Account = "999999999999"
			case "instance":
				i.ID = "i-fffffffffffffffff"
			case "zone":
				i.Zone = "us-east-1b"
			case "fleet":
				i.Tags[FleetTag] = "other"
			case "host":
				i.Tags[HostTag] = "other"
			case "boot":
				i.Tags[BootTag] = "other"
			case "optout":
				delete(i.Tags, FenceTag)
			case "delete":
				i.Disks[0].DeleteOnTermination = true
			case "missing":
				i.Disks = nil
			case "otherdisk":
				i.Disks = append(i.Disks, Disk{ID: "other"})
			case "ambiguousdisk":
				i.Disks = append(i.Disks, i.Disks[0])
			case "absent":
				i.State = ""
			case "terminated-without-intent", "terminated-with-intent":
				i.State = "terminated"
				i.Disks = nil
			}
			err := Check(b, i, fault == "terminated-with-intent")
			want := fault == "valid" || fault == "terminated-with-intent"
			if (err == nil) != want {
				t.Fatalf("Check=%v want acceptance=%v", err, want)
			}
		})
	}
}
