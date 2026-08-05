package modtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// getJSON performs a GET with retries (3 attempts, backoff), decoding the body
// into dst. headers is applied to the request. A 404 returns ErrNotFound.
func (r *Registry) getJSON(ctx context.Context, url string, headers map[string]string, dst any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", r.ua)
		req.Header.Set("Accept", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := r.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("registry %s: HTTP %d", url, resp.StatusCode)
			continue
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("registry %s: HTTP %d: %s", url, resp.StatusCode, truncate(body, 200))
		}
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("decode %s: %w", url, err)
		}
		return nil
	}
	return fmt.Errorf("registry %s: %w", url, lastErr)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
