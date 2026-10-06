package failover

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

var ErrFencingPending = errors.New("EC2 stop not yet confirmed")

type Target struct{ ID, InstanceID, Address string }
type Fencer interface {
	Fence(context.Context, Target) error
}
type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
}
type EC2Fencer struct{ Client EC2API }

func (f EC2Fencer) Fence(ctx context.Context, target Target) error {
	if target.InstanceID == "" {
		return errors.New("worker has no EC2 identity")
	}
	output, err := f.Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{target.InstanceID}})
	if err != nil {
		return err
	}
	if len(output.Reservations) != 1 || len(output.Reservations[0].Instances) != 1 {
		return errors.New("EC2 instance identity is ambiguous")
	}
	instance := output.Reservations[0].Instances[0]
	tags := map[string]string{}
	for _, tag := range instance.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	address, err := url.Parse(target.Address)
	if err != nil || tags["launchpad:role"] != "worker" || tags["launchpad:worker-id"] != target.ID || aws.ToString(instance.InstanceId) != target.InstanceID || aws.ToString(instance.PrivateIpAddress) != address.Hostname() {
		return errors.New("EC2 worker tags or private address do not match registration")
	}
	if instance.State == nil {
		return errors.New("EC2 did not report instance state")
	}
	switch instance.State.Name {
	case types.InstanceStateNameStopped:
		return nil
	case types.InstanceStateNameRunning:
		_, err := f.Client.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{target.InstanceID}})
		if err != nil {
			return fmt.Errorf("stop worker EC2: %w", err)
		}
	}
	// A successful StopInstances response is not proof that execution has stopped.
	return ErrFencingPending
}
