package v1alpha1

import "fmt"

// CapacityPolicy uses absolute per-container usage, not utilization divided by pending pods.
// +kubebuilder:validation:XValidation:rule="self.minReplicas <= self.maxReplicas",message="invalid replica bounds"
// +kubebuilder:validation:XValidation:rule="self.cpuLowMillicores < self.cpuHighMillicores && self.memoryLowMiB < self.memoryHighMiB",message="low thresholds must be below high thresholds"
// +kubebuilder:validation:XValidation:rule="self.minWindowSeconds <= self.maxWindowSeconds && self.sampleIntervalSeconds <= self.maxAgeSeconds",message="invalid sampling windows"
type CapacityPolicy struct {
	// +kubebuilder:default=Shadow
	// +kubebuilder:validation:Enum=Shadow;ScaleOut;Automatic
	Mode string `json:"mode,omitempty"`
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	ScaleOutStep int32 `json:"scaleOutStep,omitempty"`
	// +kubebuilder:default=15
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=300
	SampleIntervalSeconds int32 `json:"sampleIntervalSeconds,omitempty"`
	// +kubebuilder:default=45
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=600
	MaxAgeSeconds int32 `json:"maxAgeSeconds,omitempty"`
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	MinWindowSeconds int32 `json:"minWindowSeconds,omitempty"`
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=300
	MaxWindowSeconds int32 `json:"maxWindowSeconds,omitempty"`
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=100
	MinSamples int32 `json:"minSamples,omitempty"`
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	ScaleOutStabilizationSeconds int32 `json:"scaleOutStabilizationSeconds,omitempty"`
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	ScaleInStabilizationSeconds int32 `json:"scaleInStabilizationSeconds,omitempty"`
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	ScaleOutCooldownSeconds int32 `json:"scaleOutCooldownSeconds,omitempty"`
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	ScaleInCooldownSeconds int32 `json:"scaleInCooldownSeconds,omitempty"`
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	ProvisioningTimeoutSeconds int32 `json:"provisioningTimeoutSeconds,omitempty"`
	// +kubebuilder:default=200
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	CPUHighMillicores int32 `json:"cpuHighMillicores,omitempty"`
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	CPULowMillicores int32 `json:"cpuLowMillicores,omitempty"`
	// +kubebuilder:default=768
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	MemoryHighMiB int32 `json:"memoryHighMiB,omitempty"`
	// +kubebuilder:default=384
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	MemoryLowMiB int32 `json:"memoryLowMiB,omitempty"`
}

type CapacityStatus struct {
	Mode            string `json:"mode,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Message         string `json:"message,omitempty"`
	DesiredReplicas int32  `json:"desiredReplicas,omitempty"`
	UsefulReplicas  int32  `json:"usefulReplicas,omitempty"`
	PendingReplicas int32  `json:"pendingReplicas,omitempty"`
	CoveredReplicas int32  `json:"coveredReplicas,omitempty"`
}

func (p *CapacityPolicy) Default() {
	if p.Mode == "" {
		p.Mode = "Shadow"
	}
	if p.MinReplicas == 0 {
		p.MinReplicas = 3
	}
	if p.MaxReplicas == 0 {
		p.MaxReplicas = 10
	}
	if p.ScaleOutStep == 0 {
		p.ScaleOutStep = 1
	}
	if p.SampleIntervalSeconds == 0 {
		p.SampleIntervalSeconds = 15
	}
	if p.MaxAgeSeconds == 0 {
		p.MaxAgeSeconds = 45
	}
	if p.MinWindowSeconds == 0 {
		p.MinWindowSeconds = 5
	}
	if p.MaxWindowSeconds == 0 {
		p.MaxWindowSeconds = 60
	}
	if p.MinSamples == 0 {
		p.MinSamples = 3
	}
	if p.ScaleOutStabilizationSeconds == 0 {
		p.ScaleOutStabilizationSeconds = 30
	}
	if p.ScaleInStabilizationSeconds == 0 {
		p.ScaleInStabilizationSeconds = 600
	}
	if p.ScaleOutCooldownSeconds == 0 {
		p.ScaleOutCooldownSeconds = 300
	}
	if p.ScaleInCooldownSeconds == 0 {
		p.ScaleInCooldownSeconds = 900
	}
	if p.ProvisioningTimeoutSeconds == 0 {
		p.ProvisioningTimeoutSeconds = 600
	}
	if p.CPUHighMillicores == 0 {
		p.CPUHighMillicores = 200
	}
	if p.CPULowMillicores == 0 {
		p.CPULowMillicores = 80
	}
	if p.MemoryHighMiB == 0 {
		p.MemoryHighMiB = 768
	}
	if p.MemoryLowMiB == 0 {
		p.MemoryLowMiB = 384
	}
}
func (p *CapacityPolicy) Validate() error {
	if p.Mode != "Shadow" && p.Mode != "ScaleOut" && p.Mode != "Automatic" {
		return fmt.Errorf("invalid capacity mode")
	}
	if p.MinReplicas < 1 || p.MinReplicas > 100 {
		return fmt.Errorf("capacity.minReplicas must be 1..100")
	}
	if p.MaxReplicas < 1 || p.MaxReplicas > 100 {
		return fmt.Errorf("capacity.maxReplicas must be 1..100")
	}
	if p.ScaleOutStep < 1 || p.ScaleOutStep > 10 {
		return fmt.Errorf("capacity.scaleOutStep must be 1..10")
	}
	if p.SampleIntervalSeconds < 5 || p.SampleIntervalSeconds > 300 {
		return fmt.Errorf("capacity.sampleIntervalSeconds must be 5..300")
	}
	if p.MaxAgeSeconds < 10 || p.MaxAgeSeconds > 600 {
		return fmt.Errorf("capacity.maxAgeSeconds must be 10..600")
	}
	if p.MinWindowSeconds < 1 || p.MinWindowSeconds > 60 {
		return fmt.Errorf("capacity.minWindowSeconds must be 1..60")
	}
	if p.MaxWindowSeconds < 5 || p.MaxWindowSeconds > 300 {
		return fmt.Errorf("capacity.maxWindowSeconds must be 5..300")
	}
	if p.MinSamples < 2 || p.MinSamples > 100 {
		return fmt.Errorf("capacity.minSamples must be 2..100")
	}
	if p.ScaleOutStabilizationSeconds < 10 || p.ScaleOutStabilizationSeconds > 3600 {
		return fmt.Errorf("capacity.scaleOutStabilizationSeconds must be 10..3600")
	}
	if p.ScaleInStabilizationSeconds < 60 || p.ScaleInStabilizationSeconds > 86400 {
		return fmt.Errorf("capacity.scaleInStabilizationSeconds must be 60..86400")
	}
	if p.ScaleOutCooldownSeconds < 30 || p.ScaleOutCooldownSeconds > 86400 {
		return fmt.Errorf("capacity.scaleOutCooldownSeconds must be 30..86400")
	}
	if p.ScaleInCooldownSeconds < 60 || p.ScaleInCooldownSeconds > 86400 {
		return fmt.Errorf("capacity.scaleInCooldownSeconds must be 60..86400")
	}
	if p.ProvisioningTimeoutSeconds < 60 || p.ProvisioningTimeoutSeconds > 86400 {
		return fmt.Errorf("capacity.provisioningTimeoutSeconds must be 60..86400")
	}
	if p.CPUHighMillicores < 1 || p.CPUHighMillicores > 1000000 {
		return fmt.Errorf("capacity.cpuHighMillicores must be 1..1000000")
	}
	if p.CPULowMillicores < 1 || p.CPULowMillicores > 1000000 {
		return fmt.Errorf("capacity.cpuLowMillicores must be 1..1000000")
	}
	if p.MemoryHighMiB < 1 || p.MemoryHighMiB > 1048576 {
		return fmt.Errorf("capacity.memoryHighMiB must be 1..1048576")
	}
	if p.MemoryLowMiB < 1 || p.MemoryLowMiB > 1048576 {
		return fmt.Errorf("capacity.memoryLowMiB must be 1..1048576")
	}
	if p.MinReplicas > p.MaxReplicas || p.CPULowMillicores >= p.CPUHighMillicores || p.MemoryLowMiB >= p.MemoryHighMiB || p.MinWindowSeconds > p.MaxWindowSeconds || p.SampleIntervalSeconds > p.MaxAgeSeconds {
		return fmt.Errorf("inconsistent capacity bounds, thresholds or windows")
	}
	return nil
}
