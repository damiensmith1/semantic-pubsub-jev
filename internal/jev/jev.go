// Package jev is a client for TypeSafe's Jev model.
//
// Jev is a System One model: it returns typed answers with calibrated
// probabilities rather than generated text. It ingests the state once and
// evaluates every question against it in parallel, which is what makes
// asking one question per subscriber affordable — see
// docs/background.md for the measurements.
//
// Only the Noul primitive is implemented, because that is what routing
// needs: an independent probability per subscriber, so that several can
// match the same message. A Choice would force exactly one winner.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is TypeSafe's API root.
const DefaultBaseURL = "https://api.typesafe.ai"

// DefaultModel is the alias tracking the most recent stable release.
//
// The response reports the versioned ID that actually answered, so a run's
// results can be attributed to a specific model even when the alias moves.
const DefaultModel = "jev-latest"

// Options configures a Client.
type Options struct {
	// APIKey is required.
	APIKey string

	// Model defaults to DefaultModel. Pin a versioned ID such as
	// "jev-1.13.0" when thresholds have been tuned against it, since an
	// alias moves on its own schedule.
	Model string

	// BaseURL defaults to DefaultBaseURL. Overridden in tests.
	BaseURL string

	// HTTPClient defaults to a client with no timeout of its own: request
	// deadlines come from the context, so a caller that already bounds
	// the judge does not have two competing limits.
	HTTPClient *http.Client

	// MaxRetries bounds retries for 429 and 5xx. Zero means 2.
	MaxRetries int
}

// Client talks to the Jev API. It is safe for concurrent use.
type Client struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
	maxRetries int
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("jev: APIKey is required")
	}
	c := &Client{
		apiKey:     opts.APIKey,
		model:      opts.Model,
		baseURL:    opts.BaseURL,
		httpClient: opts.HTTPClient,
		maxRetries: opts.MaxRetries,
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	if c.maxRetries == 0 {
		c.maxRetries = 2
	}
	return c, nil
}

// Noul is a yes/no question. The answer is the probability that the
// condition holds.
type Noul struct {
	// Instructions states the judgment to make.
	Instructions string

	// True and False describe what each outcome looks like. Both are
	// required by the API; leaving one empty makes the question
	// one-sided and the answers less well calibrated.
	True  string
	False string
}

// Request is one call: a shared state, and questions evaluated against it
// in parallel.
type Request struct {
	// State is the material every question is judged against. A JSON
	// object with named fields reads better to the model than a blob.
	State any

	// Questions are keyed by an ID that is meaningful to the caller. IDs
	// are not sent to the model, so each question must carry its full
	// meaning in Instructions.
	Questions map[string]Noul
}

// Usage reports what a call cost. Output tokens are free and reported
// only for completeness.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Cost returns the dollar cost of a call at the published rate for input
// tokens. It is an estimate for budgeting, not a billing figure.
func (u Usage) Cost() float64 {
	const perMtokUSD = 0.042
	return float64(u.InputTokens) / 1e6 * perMtokUSD
}

// Response is one call's answers.
type Response struct {
	// Model is the versioned ID that answered, which may differ from the
	// alias that was requested.
	Model string

	// Probabilities are the Noul answers, keyed by the request's question
	// IDs. A question whose answer is missing is absent from the map
	// rather than present as zero, so "no answer" and "certainly not"
	// stay distinguishable.
	Probabilities map[string]float64

	Usage   Usage
	Latency time.Duration
}

// --- wire types -----------------------------------------------------------

type wireQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type wireRequest struct {
	Model     string                  `json:"model"`
	State     any                     `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireAnswer struct {
	Type string  `json:"type"`
	Noul float64 `json:"noul"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   Usage                 `json:"usage"`
}

// --- errors ---------------------------------------------------------------

// APIError is a non-success HTTP response.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jev: api returned %d: %s", e.StatusCode, truncate(e.Body, 300))
}

// Retryable reports whether retrying the same request could succeed.
// Rate limits and server faults can; a malformed or unauthorised request
// cannot, and retrying it just burns the budget faster.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- the call -------------------------------------------------------------

// Ask evaluates every question against the state in a single request.
//
// Questions run in parallel and cannot see one another's answers, so they
// must be independent. Latency is near-flat in question count while cost
// is linear, which is why callers should batch rather than loop.
func (c *Client) Ask(ctx context.Context, req Request) (*Response, error) {
	if len(req.Questions) == 0 {
		return nil, errors.New("jev: Ask needs at least one question")
	}

	body, err := json.Marshal(c.toWire(req))
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt, lastErr)); err != nil {
				return nil, err
			}
		}

		resp, err := c.attempt(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			return nil, err
		}
		// Context cancellation is never worth retrying.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("jev: giving up after %d attempts: %w", c.maxRetries+1, lastErr)
}

func (c *Client) toWire(req Request) wireRequest {
	qs := make(map[string]wireQuestion, len(req.Questions))
	for id, n := range req.Questions {
		qs[id] = wireQuestion{
			Type:         "noul",
			Instructions: n.Instructions,
			Criteria:     map[string]string{"true": n.True, "false": n.False},
		}
	}
	return wireRequest{Model: c.model, State: req.State, Questions: qs}
}

func (c *Client) attempt(ctx context.Context, body []byte) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jev: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jev: request failed: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("jev: read response: %w", err)
	}
	latency := time.Since(start)

	if httpResp.StatusCode != http.StatusOK {
		e := &APIError{StatusCode: httpResp.StatusCode, Body: string(raw)}
		if d, ok := retryAfter(httpResp); ok {
			return nil, &retryAfterError{APIError: e, wait: d}
		}
		return nil, e
	}

	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("jev: decode response: %w (body %s)", err, truncate(string(raw), 300))
	}

	probs := make(map[string]float64, len(wire.Answers))
	for id, a := range wire.Answers {
		probs[id] = a.Noul
	}
	return &Response{
		Model:         wire.Model,
		Probabilities: probs,
		Usage:         wire.Usage,
		Latency:       latency,
	}, nil
}

// retryAfterError carries the server's own backoff instruction.
type retryAfterError struct {
	*APIError
	wait time.Duration
}

// Unwrap exposes the underlying APIError to errors.As.
//
// Embedding alone is not enough: errors.As matches on concrete type, and
// *retryAfterError is not *APIError. Without this, every response
// carrying Retry-After — which is precisely the rate-limit case retries
// exist for — looks non-retryable and fails on the first attempt.
func (e *retryAfterError) Unwrap() error { return e.APIError }

func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// backoff prefers the server's Retry-After over a guess, since the server
// knows when the limit actually resets.
func backoff(attempt int, lastErr error) time.Duration {
	var ra *retryAfterError
	if errors.As(lastErr, &ra) {
		return ra.wait
	}
	d := time.Duration(math.Pow(2, float64(attempt))) * 250 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
