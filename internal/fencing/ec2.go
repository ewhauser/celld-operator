// Package fencing provides exact-instance infrastructure fencing. A termination
// request is not proof: only a positive, matching terminated instance is a fence.
package fencing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

const FleetTag = "celld.eric.dev/fleet-uid"
const HostTag = "celld.eric.dev/node-uid"
const BootTag = "celld.eric.dev/boot-id"
const FenceTag = "celld.eric.dev/fencing"

// Binding is captured from an authenticated admitted runtime and retained disk.
// Ownership tags are installed by trusted infrastructure provisioning, not this operator.
type Binding struct {
	Account, Region, Zone, Instance, FleetUID, HostUID, BootID, Volume string
}
type Disk struct {
	ID                  string
	DeleteOnTermination bool
	Root                bool
}
type Instance struct {
	Account, ID, Zone, State string
	Tags                     map[string]string
	Disks                    []Disk
}
type API interface {
	Describe(context.Context, string) (Instance, error)
	Terminate(context.Context, string) error
}
type Client struct{ api *ec2.Client }

func New(ctx context.Context, region string) (*Client, error) {
	if !regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`).MatchString(region) {
		return nil, errors.New("invalid fencing region")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	// SDK credentials are used, but an arbitrary configured endpoint may not attest EC2 state.
	cfg.BaseEndpoint = nil
	return &Client{api: ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		o.BaseEndpoint = aws.String("https://ec2." + region + ".amazonaws.com")
		o.RetryMaxAttempts = 2
	})}, nil
}
func (c *Client) Describe(ctx context.Context, id string) (Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return Instance{}, err
	}
	if len(out.Reservations) != 1 || len(out.Reservations[0].Instances) != 1 || out.NextToken != nil {
		return Instance{}, errors.New("ambiguous EC2 instance response")
	}
	reservation := out.Reservations[0]
	item := reservation.Instances[0]
	if item.Placement == nil || item.State == nil {
		return Instance{}, errors.New("EC2 identity incomplete")
	}
	result := Instance{Account: aws.ToString(reservation.OwnerId), ID: aws.ToString(item.InstanceId), Zone: aws.ToString(item.Placement.AvailabilityZone), State: string(item.State.Name), Tags: map[string]string{}}
	for _, tag := range item.Tags {
		key := aws.ToString(tag.Key)
		if _, exists := result.Tags[key]; exists {
			return Instance{}, errors.New("duplicate EC2 tag")
		}
		result.Tags[key] = aws.ToString(tag.Value)
	}
	for _, disk := range item.BlockDeviceMappings {
		if disk.Ebs != nil {
			result.Disks = append(result.Disks, Disk{ID: aws.ToString(disk.Ebs.VolumeId), DeleteOnTermination: aws.ToBool(disk.Ebs.DeleteOnTermination), Root: aws.ToString(disk.DeviceName) == aws.ToString(item.RootDeviceName)})
		}
	}
	return result, nil
}
func (c *Client) Terminate(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	return err
}

func (b Binding) Validate() error {
	if !regexp.MustCompile(`^\d{12}$`).MatchString(b.Account) || !regexp.MustCompile(`^i-[0-9a-f]{17}$`).MatchString(b.Instance) || !regexp.MustCompile(`^vol-[0-9a-f]{17}$`).MatchString(b.Volume) || b.Region == "" || b.Zone == "" || b.FleetUID == "" || b.HostUID == "" || b.BootID == "" {
		return errors.New("incomplete exact infrastructure fence binding")
	}
	return nil
}

// Check requires live retained-disk attachment before issuance. Once intent is
// durable, a terminated instance may no longer list its former attachments.
func Check(b Binding, i Instance, intent bool) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if i.Account != b.Account || i.ID != b.Instance || i.Zone != b.Zone || i.Tags[FleetTag] != b.FleetUID || i.Tags[HostTag] != b.HostUID || i.Tags[BootTag] != b.BootID || i.Tags[FenceTag] != "terminate" {
		return errors.New("EC2 ownership or incarnation differs from admitted writer")
	}
	if i.State == "terminated" && intent {
		return nil
	}
	found := false
	for _, d := range i.Disks {
		if d.ID != b.Volume && !d.Root {
			return errors.New("another data volume is attached to the instance")
		}
		if d.ID == b.Volume {
			if found || d.DeleteOnTermination {
				return errors.New("retained data volume could be deleted on termination")
			}
			found = true
		}
	}
	if !found {
		return errors.New("captured data volume is not attached to exact instance")
	}
	switch i.State {
	case "running", "stopped", "stopping", "shutting-down":
		return nil
	default:
		return fmt.Errorf("unsupported EC2 fencing state %q", i.State)
	}
}
