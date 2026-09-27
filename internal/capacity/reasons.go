package capacity

// Reasons is every status.capacity.reason the policy and its controller emit.
// Metrics label decisions from this fixed set, so a reason added to Evaluate,
// assessAddition or capacityTarget must be added here too; a test enforces it.
var Reasons = []string{
	// Recommendations and stabilization.
	"ScaleOutRecommended", "ScaleInRecommended", "StabilizingOut", "StabilizingIn",
	"WithinThresholds", "AtMinimum", "AtMaximum",
	// Capacity and observation problems.
	"PendingCapacity", "IneffectiveCapacity", "IncompleteMetrics",
	"InvalidObservation", "RepeatedSamples", "WindowInterrupted",
	// Other holds.
	"PolicyChanged", "RateLimited", "ManualOverride", "ExternalOwner",
	// Redistribution.
	"ObservingRedistribution", "LoadNotRedistributed",
}
