// Package inventory is an HTTP client for the inventory service.
package inventory

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *Client) Reserve(ctx context.Context, sku string, qty int) error {
	resp, err := c.get(ctx, "/items/"+sku)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("inventory: status %d reserving %d x %s", resp.StatusCode, qty, sku)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.http.Do(req)
}
