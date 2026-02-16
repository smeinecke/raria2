package raria2

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewHTTPClient(t *testing.T) {
	c := NewHTTPClient(5*time.Second, 0)
	assert.NotNil(t, c)
	assert.NotNil(t, c.client)
	assert.Equal(t, 3, c.retryCount)
}

func TestNewHTTPClientDefaultTimeout(t *testing.T) {
	c := NewHTTPClient(0, 0)
	assert.NotNil(t, c)
}

func TestHTTPClientDoWithRetryTransientError(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	c := NewHTTPClient(2*time.Second, 0)
	c.client = server.Client()
	c.baseDelay = 10 * time.Millisecond

	req, _ := http.NewRequest("GET", server.URL, nil)
	resp, err := c.DoWithRetry(req)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	assert.Equal(t, 3, attempts)
}

func TestHTTPClientDoNoRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	c := NewHTTPClient(2*time.Second, 0)
	c.client = server.Client()

	req, _ := http.NewRequest("GET", server.URL, nil)
	resp, err := c.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestWaitForRateLimitConcurrentSafety(t *testing.T) {
	c := NewHTTPClient(5*time.Second, 100) // 100 rps = 10ms interval

	var wg sync.WaitGroup
	var count atomic.Int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.waitForRateLimit()
			count.Add(1)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(5), count.Load())
}

func TestWaitForRateLimitZeroRate(t *testing.T) {
	c := NewHTTPClient(5*time.Second, 0)
	// Should return immediately without panic.
	c.waitForRateLimit()
}

func TestRAriaClientInitOnce(t *testing.T) {
	r := &RAria2{HTTPTimeout: 2 * time.Second, RateLimit: 0}

	var wg sync.WaitGroup
	clients := make([]*HTTPClient, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			clients[idx] = r.client()
		}(i)
	}
	wg.Wait()

	// All goroutines must get the same instance.
	for i := 1; i < 10; i++ {
		assert.Same(t, clients[0], clients[i])
	}
}

func TestRAriaClientRespectsPreset(t *testing.T) {
	preset := NewHTTPClient(1*time.Second, 0)
	r := &RAria2{httpClient: preset}
	assert.Same(t, preset, r.client())
}
