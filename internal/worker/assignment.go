package worker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type StartGuard func(context.Context, string, int) error

// AssignmentGuard fails closed when the authoritative assignment cannot be read.
func AssignmentGuard(origin, workerID, token string) StartGuard {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, id string, version int) error {
		address := fmt.Sprintf("%s/internal/workers/%s/assignments/%s?version=%d", strings.TrimRight(origin, "/"), workerID, id, version)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("assignment check returned HTTP %d", response.StatusCode)
		}
		return nil
	}
}
