package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/workers"
)

type ResourceSource interface {
	Resources(context.Context) (containers.Resources, error)
}

// ReportHeartbeats registers immediately and retries on every interval. The ID
// comes from configuration so a container restart updates the same database row.
func ReportHeartbeats(ctx context.Context, baseURL, token string, identity workers.Report, source ResourceSource, interval time.Duration, logger *slog.Logger) {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	registered := false
	for {
		report := identity
		sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resources, err := source.Resources(sampleCtx)
		cancel()
		report.Healthy = err == nil
		if err == nil {
			report.TotalCPU = float64(resources.CPUs)
			report.AvailableCPU = resources.AvailableCPU
			report.TotalMemory = resources.MemoryBytes
			report.AvailableMemory = resources.AvailableMemory
			report.RunningContainers = resources.ContainersRunning
		} else if ctx.Err() == nil {
			logger.Warn("worker resource sample failed", "error", err)
		}
		endpoint := "/internal/workers/heartbeat"
		if !registered {
			endpoint = "/internal/workers/register"
		}
		if err := sendHeartbeat(ctx, client, strings.TrimRight(baseURL, "/")+endpoint, token, report); err != nil {
			if ctx.Err() == nil {
				logger.Warn("worker heartbeat failed", "error", err)
			}
		} else {
			registered = true
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func sendHeartbeat(ctx context.Context, client *http.Client, endpoint, token string, report workers.Report) error {
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("heartbeat returned HTTP %d", res.StatusCode)
	}
	return nil
}
