package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/workers"
)

type RecoveryManager interface {
	LogTail(context.Context, string) (string, error)
	Status(context.Context, string, int) (containers.Status, error)
	Restart(context.Context, string) error
}
type ApplicationMonitor struct {
	Manager                               RecoveryManager
	RegistryURL, WorkerID, Token, AppHost string
	Client                                *http.Client
	Logger                                *slog.Logger
}

func (m ApplicationMonitor) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := m.Scan(ctx); err != nil && ctx.Err() == nil {
			m.Logger.Warn("application health scan failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (m ApplicationMonitor) endpoint() string {
	return strings.TrimRight(m.RegistryURL, "/") + "/internal/workers/" + m.WorkerID + "/applications"
}
func (m ApplicationMonitor) Scan(ctx context.Context) error {
	var items []deployments.Deployment
	if err := m.registry(ctx, http.MethodGet, nil, &items); err != nil {
		return err
	}
	for _, d := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := m.check(ctx, d); err != nil {
			m.Logger.Warn("application monitor error", "deployment_id", d.ID, "error", err)
		}
	}
	return nil
}
func (m ApplicationMonitor) check(ctx context.Context, d deployments.Deployment) error {
	lock := lifecycleLock(d.ID)
	lock.Lock()
	defer lock.Unlock()
	if d.ContainerID == nil {
		return nil
	}
	status, err := m.Manager.Status(ctx, d.ID, d.Version)
	if err != nil {
		return err
	} // A Docker API outage is not evidence of an application crash.
	o := workers.Observation{DeploymentID: d.ID, Version: d.Version, ContainerID: *d.ContainerID, ExpectedAttempts: d.RestartAttempts, State: "RUNNING"}
	if status.ContainerID != *d.ContainerID {
		o.State = "UNHEALTHY"
		o.Message = "managed container missing or identity changed; automatic recreation is not supported"
	} else if !status.Running {
		o.State = "EXITED"
		o.ExitCode = status.ExitCode
		o.OOMKilled = status.OOMKilled
		o.Message = fmt.Sprintf("container exited with code %d (oom_killed=%t)", status.ExitCode, status.OOMKilled)
		if status.Error != "" {
			o.Message += "; " + status.Error
		}
		o.RequestRestart = true
		if logs, err := m.Manager.LogTail(ctx, *d.ContainerID); err == nil {
			o.LogTail = logs
		}
	} else if d.HealthPath != "" {
		if err := m.probe(ctx, status.HostPort, d.HealthPath); err != nil {
			o.State = "UNHEALTHY"
			o.Message = err.Error()
		}
	}
	if len(o.Message) > 2000 {
		o.Message = o.Message[:2000]
	}
	var decision workers.RecoveryDecision
	// Record/claim before restart. A lost response burns an attempt rather than
	// replaying an uncertain restart and exceeding the durable budget.
	if err := m.registry(ctx, http.MethodPost, o, &decision); err != nil {
		return err
	}
	if decision.Restart {
		m.Logger.Warn("restarting exited application", "deployment_id", d.ID, "attempt", d.RestartAttempts+1)
		if err := m.Manager.Restart(ctx, *d.ContainerID); err != nil {
			o.ExpectedAttempts++
			o.RequestRestart = false
			o.Message = "restart command failed: " + err.Error()
			if len(o.Message) > 2000 {
				o.Message = o.Message[:2000]
			}
			return m.registry(ctx, http.MethodPost, o, &workers.RecoveryDecision{})
		}
	}
	return nil
}
func (m ApplicationMonitor) registry(ctx context.Context, method string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.endpoint(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("application registry returned HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(output)
}
func (m ApplicationMonitor) probe(ctx context.Context, port int, path string) error {
	if err := deployments.ValidateHealthPath(path); err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid health check port")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	relative, _ := url.ParseRequestURI(path)
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(m.AppHost, strconv.Itoa(port)), Path: relative.Path, RawPath: relative.RawPath, RawQuery: relative.RawQuery}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	// No worker token is forwarded to generated code. Redirects are not followed.
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP health check failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP health check returned %d", res.StatusCode)
	}
	return nil
}
