package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrRejected means the backend refused a report for what it says, so sending it again cannot succeed.
var ErrRejected = errors.New("backend rejected the report")

const secretHeader = "X-Internal-Secret"

// Client posts reports to the backend's internal API, authenticated with the shared service secret.
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// NewClient builds a client for the backend at baseURL.
func NewClient(baseURL, secret string) *Client {
	return &Client{baseURL: baseURL, secret: secret, http: &http.Client{Timeout: 10 * time.Second}}
}

// Send delivers one report. It returns ErrRejected for a report the backend will never accept, and any other error for a failure worth retrying.
func (c *Client) Send(ctx context.Context, report Report) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+report.Path, bytes.NewReader(report.Body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(secretHeader, c.secret)

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("reaching the backend: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	switch status := response.StatusCode; {
	case status < 300:
		return nil
	case status == http.StatusBadRequest, status == http.StatusNotFound, status == http.StatusConflict:
		return fmt.Errorf("%w: status %d", ErrRejected, status)
	default:
		// Includes 401: a wrong secret is a deployment mistake, and the reports must survive until it is fixed
		return fmt.Errorf("backend answered status %d", status)
	}
}
