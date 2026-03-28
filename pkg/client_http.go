package raria2

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// HTTPClient wraps the standard HTTP client with retry logic and rate limiting
type HTTPClient struct {
	client      *http.Client
	rateLimit   float64
	lastRequest time.Time
	rateMu      sync.Mutex
	retryCount  int
	baseDelay   time.Duration
}

// NewHTTPClient creates a new HTTP client with the specified timeout and rate limit
func NewHTTPClient(timeout time.Duration, rateLimit float64) *HTTPClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &HTTPClient{
		client:     &http.Client{Timeout: timeout},
		rateLimit:  rateLimit,
		retryCount: 3,
		baseDelay:  100 * time.Millisecond,
	}
}

// Do performs an HTTP request with retry logic and rate limiting
func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	return c.doWithRetry(req, false)
}

// DoWithRetry performs an HTTP request with retry logic and rate limiting
func (c *HTTPClient) DoWithRetry(req *http.Request) (*http.Response, error) {
	return c.doWithRetry(req, true)
}

// doWithRetry implements the retry logic
func (c *HTTPClient) doWithRetry(req *http.Request, enableRetries bool) (*http.Response, error) {
	// If retries are disabled, just do a single request
	if !enableRetries {
		c.waitForRateLimit()
		return c.client.Do(req)
	}

	var lastErr error

	for attempt := 0; attempt < c.retryCount; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			delay := c.baseDelay * time.Duration(1<<uint(attempt-1))
			jitter := time.Duration(float64(delay) * 0.1 * (0.5 + 0.5*rand.Float64()))
			time.Sleep(delay + jitter)

			logrus.Debugf("Retrying HTTP request (attempt %d/%d)", attempt+1, c.retryCount)
		}

		c.waitForRateLimit()

		resp, err := c.client.Do(req)
		if err == nil {
			// Check for transient HTTP errors
			if resp.StatusCode >= 500 || resp.StatusCode == 429 {
				lastErr = fmt.Errorf("HTTP %d: transient error", resp.StatusCode)
				resp.Body.Close()
				continue
			}
			return resp, nil
		}

		// Check if error is transient (network-related)
		if IsTransientError(err) {
			lastErr = err
			continue
		}

		// Non-transient error, return immediately
		return nil, err
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// waitForRateLimit implements rate limiting with proper handling of fractional rates.
// A mutex serializes the check-and-sleep so concurrent goroutines cannot bypass the
// interval by reading the same lastRequest timestamp simultaneously.
func (c *HTTPClient) waitForRateLimit() {
	if c.rateLimit <= 0 {
		return
	}

	minInterval := time.Duration(float64(time.Second) / c.rateLimit)
	if minInterval <= 0 {
		return
	}

	c.rateMu.Lock()
	elapsed := time.Since(c.lastRequest)
	if elapsed < minInterval {
		c.rateMu.Unlock()
		time.Sleep(minInterval - elapsed)
		c.rateMu.Lock()
	}
	c.lastRequest = time.Now()
	c.rateMu.Unlock()
}

// IsTransientError checks if an error is transient and worth retrying
func IsTransientError(err error) bool {
	errStr := err.Error()

	// Common transient error patterns
	transientPatterns := []string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"timeout",
		"network is unreachable",
		"temporary failure",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
	}

	for _, pattern := range transientPatterns {
		if strings.Contains(strings.ToLower(errStr), pattern) {
			return true
		}
	}

	return false
}

// IsHTMLContent checks if content type indicates HTML
func IsHTMLContent(contentType string) bool {
	contentTypeEntries := strings.Split(contentType, ";")
	for _, v := range contentTypeEntries {
		if strings.TrimSpace(v) == "text/html" {
			return true
		}
	}
	return false
}

func (r *RAria2) client() *HTTPClient {
	r.httpClientOnce.Do(func() {
		if r.httpClient != nil {
			return
		}
		timeout := r.HTTPTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		r.httpClient = NewHTTPClient(timeout, r.RateLimit)
		if r.SkipCertificateCheck {
			baseTransport, ok := http.DefaultTransport.(*http.Transport)
			var transport *http.Transport
			if ok {
				transport = baseTransport.Clone()
			} else {
				transport = &http.Transport{}
			}
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			r.httpClient.client.Transport = transport
		}
	})
	return r.httpClient
}
