package routing

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workers"
)

type WorkerLookup interface {
	Get(context.Context, string) (workers.Worker, error)
}

// VerifiedResolver checks live placement only on a route-cache miss. It never
// moves deployments or changes their persisted lifecycle state.
type VerifiedResolver struct {
	Deployments      Resolver
	Workers          WorkerLookup
	Client           func(string) (worker.Client, error)
	HeartbeatTimeout time.Duration
	LegacyAddress    string
}

func (v VerifiedResolver) GetByPublicIdentifier(ctx context.Context, id string) (deployments.Deployment, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := v.Deployments.GetByPublicIdentifier(ctx, id)
	if err != nil {
		return d, err
	}
	if d.Status != deployments.StatusRunning || d.ContainerID == nil {
		return d, errors.New("deployment is not running")
	}
	address := v.LegacyAddress
	if d.WorkerID != nil {
		w, err := v.Workers.Get(ctx, *d.WorkerID)
		if err != nil {
			return d, errors.New("assigned worker unavailable")
		}
		if w.Status != "HEALTHY" || time.Since(w.LastHeartbeat) > v.HeartbeatTimeout {
			return d, errors.New("worker heartbeat unavailable")
		}
		if d.WorkerAddress == nil || *d.WorkerAddress != w.Address {
			return d, errors.New("worker address changed; deployment location requires verification")
		}
		address = w.Address
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return d, errors.New("invalid worker address")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsPrivate() {
		return d, errors.New("worker must have a private IP address")
	}
	client, err := v.Client(address)
	if err != nil {
		return d, err
	}
	status, err := client.Status(ctx, d.ID, d.Version)
	if err != nil || !status.Running || status.ContainerID != *d.ContainerID || status.DeploymentID != d.ID || status.Version != d.Version || status.HostPort < 1 || status.HostPort > 65535 {
		return d, errors.New("container unavailable or identity changed")
	}
	d.WorkerAddress = &address
	d.HostPort = &status.HostPort
	return d, nil
}
