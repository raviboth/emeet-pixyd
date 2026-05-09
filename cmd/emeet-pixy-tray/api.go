package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"
)

type apiClient struct {
	baseURL string
	hc      *http.Client
}

func newAPIClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL: baseURL,
		hc:      &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *apiClient) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("post %s: status %d", path, resp.StatusCode)
	}
	return nil
}

func (c *apiClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/panel", http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ping: status %d", resp.StatusCode)
	}
	return nil
}

func openBrowser(url string) {
	_ = exec.Command("xdg-open", url).Start()
}
