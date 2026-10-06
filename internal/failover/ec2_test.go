package failover

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"testing"
)

type fakeEC2 struct {
	instance types.Instance
	stops    int
	err      error
}

func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{f.instance}}}}, f.err
}
func (f *fakeEC2) StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.stops++
	return &ec2.StopInstancesOutput{}, f.err
}
func TestEC2RequiresConfirmedStopAndIdentity(t *testing.T) {
	target := Target{ID: "worker-id", InstanceID: "i-12345678", Address: "http://10.0.0.1:8090"}
	for _, test := range []struct {
		name      string
		state     types.InstanceStateName
		badTag    bool
		wantStops int
		confirmed bool
	}{
		{"running", types.InstanceStateNameRunning, false, 1, false}, {"stopping", types.InstanceStateNameStopping, false, 0, false}, {"stopped", types.InstanceStateNameStopped, false, 0, true}, {"wrong identity", types.InstanceStateNameRunning, true, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeEC2{instance: types.Instance{InstanceId: aws.String(target.InstanceID), PrivateIpAddress: aws.String("10.0.0.1"), State: &types.InstanceState{Name: test.state}, Tags: []types.Tag{{Key: aws.String("launchpad:role"), Value: aws.String("worker")}, {Key: aws.String("launchpad:worker-id"), Value: aws.String(target.ID)}}}}
			if test.badTag {
				f.instance.Tags = nil
			}
			err := (EC2Fencer{Client: f}).Fence(context.Background(), target)
			if (err == nil) != test.confirmed || f.stops != test.wantStops {
				t.Fatalf("err %v stops %d", err, f.stops)
			}
			if !test.badTag && !test.confirmed && !errors.Is(err, ErrFencingPending) {
				t.Fatal(err)
			}
		})
	}
}
