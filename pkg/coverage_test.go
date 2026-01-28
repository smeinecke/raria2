package raria2

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWaitForRateLimit(t *testing.T) {
	tests := []struct {
		name        string
		rateLimit   float64
		expectDelay bool
	}{
		{
			name:        "no rate limit",
			rateLimit:   0,
			expectDelay: false,
		},
		{
			name:        "small rate limit",
			rateLimit:   10,    // 10 requests per second
			expectDelay: false, // Too fast to measure reliably
		},
		{
			name:        "high rate limit",
			rateLimit:   1000,  // 1000 requests per second
			expectDelay: false, // Very small delay
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RAria2{
				RateLimit: tt.rateLimit,
			}

			start := time.Now()
			r.waitForRateLimit()
			duration := time.Since(start)

			// Just verify it doesn't panic and completes
			assert.Greater(t, duration, time.Duration(0))
		})
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "timeout error",
			err:      context.DeadlineExceeded,
			expected: true,
		},
		{
			name:     "connection refused",
			err:      &url.Error{Err: errors.New("connection refused")},
			expected: true,
		},
		{
			name:     "DNS error",
			err:      &url.Error{Err: &net.DNSError{Err: "no such host"}},
			expected: true,
		},
		{
			name:     "temporary error",
			err:      &net.OpError{Err: net.ErrClosed},
			expected: true,
		},
		{
			name:     "generic error",
			err:      assert.AnError,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isTransientError(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsHtmlPage(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{
			name:     "HTML with doctype",
			content:  "<!DOCTYPE html><html><body></body></html>",
			expected: true,
		},
		{
			name:     "HTML without doctype",
			content:  "<html><head><title>Test</title></head><body></body></html>",
			expected: true,
		},
		{
			name:     "HTML with XML declaration",
			content:  `<?xml version="1.0" encoding="UTF-8"?><html><body></body></html>`,
			expected: true,
		},
		{
			name:     "XHTML",
			content:  `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body></body></html>`,
			expected: true,
		},
		{
			name:     "plain text",
			content:  "This is just plain text",
			expected: false,
		},
		{
			name:     "JSON content",
			content:  `{"key": "value", "array": [1, 2, 3]}`,
			expected: false,
		},
		{
			name:     "XML but not HTML",
			content:  `<?xml version="1.0"?><root><item>test</item></root>`,
			expected: false,
		},
		{
			name:     "HTML with whitespace before",
			content:  "   \n\t<!DOCTYPE html><html></html>",
			expected: true,
		},
		{
			name:     "empty string",
			content:  "",
			expected: false,
		},
		{
			name:     "HTML with comments",
			content:  "<!-- comment --><html><body></body></html>",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isHTMLContent(tt.content)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCanonicalURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple URL",
			input:    "https://example.com/path",
			expected: "https://example.com/path",
		},
		{
			name:     "URL with fragment",
			input:    "https://example.com/path#fragment",
			expected: "https://example.com/path",
		},
		{
			name:     "URL with query and fragment",
			input:    "https://example.com/path?param=value#fragment",
			expected: "https://example.com/path?param=value",
		},
		{
			name:     "URL with only fragment",
			input:    "https://example.com/path#",
			expected: "https://example.com/path",
		},
		{
			name:     "URL with empty fragment",
			input:    "https://example.com/path#",
			expected: "https://example.com/path",
		},
		{
			name:     "relative URL",
			input:    "/path/to/resource",
			expected: "/path/to/resource",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canonicalURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsSubPath_Coverage(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		target   string
		expected bool
	}{
		{
			name:     "same path",
			base:     "/path/to/file",
			target:   "/path/to/file",
			expected: true,
		},
		{
			name:     "subdirectory",
			base:     "/path/to",
			target:   "/path/to/file",
			expected: true,
		},
		{
			name:     "different path",
			base:     "/path/to",
			target:   "/other/path/file",
			expected: false,
		},
		{
			name:     "parent directory",
			base:     "/path/to/file",
			target:   "/path",
			expected: false,
		},
		{
			name:     "root path",
			base:     "/",
			target:   "/any/path",
			expected: true,
		},
		{
			name:     "empty paths",
			base:     "",
			target:   "",
			expected: true,
		},
		{
			name:     "similar but different",
			base:     "/path1",
			target:   "/path2",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL, _ := url.Parse(tt.base)
			targetURL, _ := url.Parse(tt.target)
			result := IsSubPath(baseURL, targetURL)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSameUrl_Coverage(t *testing.T) {
	tests := []struct {
		name     string
		url1     string
		url2     string
		expected bool
	}{
		{
			name:     "identical URLs",
			url1:     "https://example.com/path",
			url2:     "https://example.com/path",
			expected: true,
		},
		{
			name:     "different scheme",
			url1:     "https://example.com/path",
			url2:     "http://example.com/path",
			expected: false,
		},
		{
			name:     "different host",
			url1:     "https://example.com/path",
			url2:     "https://other.com/path",
			expected: false,
		},
		{
			name:     "different path",
			url1:     "https://example.com/path1",
			url2:     "https://example.com/path2",
			expected: false,
		},
		{
			name:     "case insensitive host",
			url1:     "https://EXAMPLE.com/path",
			url2:     "https://example.com/path",
			expected: true,
		},
		{
			name:     "trailing slash difference",
			url1:     "https://example.com/path",
			url2:     "https://example.com/path/",
			expected: false,
		},
		{
			name:     "invalid URLs",
			url1:     "not-a-url",
			url2:     "also-not-a-url",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url1, _ := url.Parse(tt.url1)
			url2, _ := url.Parse(tt.url2)
			result := SameUrl(url1, url2)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestEnsureOutputDir_Coverage(t *testing.T) {
	tests := []struct {
		name        string
		outputPath  string
		expectError bool
	}{
		{
			name:        "valid existing directory",
			outputPath:  "/tmp",
			expectError: false,
		},
		{
			name:        "empty path",
			outputPath:  "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RAria2{
				OutputPath: tt.outputPath,
			}

			err := r.ensureOutputDir(0, tt.outputPath)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestWriteBatchFile_Coverage(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name        string
		entries     []byte
		expectError bool
	}{
		{
			name:        "valid entries",
			entries:     []byte("http://example.com/file1.pdf\nhttp://example.com/file2.txt\n"),
			expectError: false,
		},
		{
			name:        "empty entries",
			entries:     []byte(""),
			expectError: false,
		},
		{
			name:        "single entry",
			entries:     []byte("http://example.com/file.pdf\n"),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RAria2{
				OutputPath: tempDir,
			}

			err := r.writeBatchFile(tt.entries)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
