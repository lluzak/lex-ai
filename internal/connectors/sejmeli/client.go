// Package sejmeli is the connector for the Sejm ELI API: Dziennik Ustaw (DU)
// and Monitor Polski (MP).
package sejmeli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

const maxBody = 200 << 20 // 200 MiB; large consolidated texts with annexes exist

var ErrNotFound = errors.New("not found")

// Client talks to the ELI API politely: one shared rate limiter for all
// goroutines and a descriptive User-Agent.
type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client
	limiter   *rate.Limiter
}

func NewClient(baseURL, userAgent string, rps float64) *Client {
	return &Client{
		BaseURL:   baseURL,
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 2 * time.Minute},
		limiter:   rate.NewLimiter(rate.Limit(rps), 1),
	}
}

func ELI(publisher string, year, pos int) string {
	return publisher + "/" + strconv.Itoa(year) + "/" + strconv.Itoa(pos)
}

// ListActs returns all acts of a publisher in a year, with the raw JSON.
func (c *Client) ListActs(ctx context.Context, publisher string, year int) (ActList, []byte, error) {
	raw, _, err := c.get(ctx, fmt.Sprintf("/acts/%s/%d", publisher, year))
	if err != nil {
		return ActList{}, nil, err
	}
	var list ActList
	if err := json.Unmarshal(raw, &list); err != nil {
		return ActList{}, raw, fmt.Errorf("decode act list %s/%d: %w", publisher, year, err)
	}
	return list, raw, nil
}

// Act returns the details JSON of one act.
func (c *Client) Act(ctx context.Context, publisher string, year, pos int) ([]byte, error) {
	raw, _, err := c.get(ctx, "/acts/"+ELI(publisher, year, pos))
	return raw, err
}

// TextHTML returns the HTML text of an act (available mostly for newer acts).
func (c *Client) TextHTML(ctx context.Context, publisher string, year, pos int) ([]byte, error) {
	raw, _, err := c.get(ctx, "/acts/"+ELI(publisher, year, pos)+"/text.html")
	return raw, err
}

// TextPDF returns the original PDF of an act.
func (c *Client) TextPDF(ctx context.Context, publisher string, year, pos int) ([]byte, error) {
	raw, _, err := c.get(ctx, "/acts/"+ELI(publisher, year, pos)+"/text.pdf")
	return raw, err
}

// URL returns the absolute URL for an API path, for provenance records.
func (c *Client) URL(path string) string { return c.BaseURL + path }

func (c *Client) get(ctx context.Context, path string) ([]byte, string, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", fmt.Errorf("GET %s: %w", path, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		// 429 and 5xx are returned as errors; the job queue retries with backoff.
		return nil, "", fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, "", fmt.Errorf("GET %s: read body: %w", path, err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}
