package raria2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsSFTPGoURL(t *testing.T) {
	assert.True(t, IsSFTPGoURL("http://example.com/web/client/files?path=/"))
	assert.True(t, IsSFTPGoURL("https://example.com/web/client/files?path=/foo"))
	assert.False(t, IsSFTPGoURL("http://example.com/some/path"))
	assert.False(t, IsSFTPGoURL("ftp://example.com/web/client/files"))
	assert.False(t, IsSFTPGoURL("not-a-url"))
}

func TestDetectSFTPGo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/login" {
			_, _ = w.Write([]byte("<html><script>$('title').text('SFTPGo 2.7 WebClient');</script></html>"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true

	detected, err := r.DetectSFTPGo(context.Background())
	assert.NoError(t, err)
	assert.True(t, detected)
}

func TestDetectSFTPGo_NotSFTPGo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/login" {
			_, _ = w.Write([]byte("<html><title>Some Other App</title></html>"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true

	detected, err := r.DetectSFTPGo(context.Background())
	assert.NoError(t, err)
	assert.False(t, detected)
}

func TestSFTPGoLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/web/client/login":
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: "jwt", Value: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJXZWJMb2dpbiJ9", Path: "/web/client"})
				_, _ = w.Write([]byte(`<form action="/web/client/login" method="POST">
					<input name="username" />
					<input name="password" />
					<input type="hidden" name="_form_token" value="formtoken123" />
					</form>`))
				return
			}
			if r.Method == http.MethodPost {
				// Verify credentials and form token
				if r.PostFormValue("username") != "gpn" || r.PostFormValue("password") != "gpn" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.PostFormValue("_form_token") != "formtoken123" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				// Check cookie presence
				cookie, err := r.Cookie("jwt")
				if err != nil || cookie.Value != "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJXZWJMb2dpbiJ9" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}

				http.SetCookie(w, &http.Cookie{Name: "jwt", Value: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJXZWJDbGllbnQifQ", Path: "/web/client"})
				w.Header().Set("Location", "/web/client/files")
				w.WriteHeader(http.StatusFound)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true
	r.SFTPGoUsername = "gpn"
	r.SFTPGoPassword = "gpn"

	err := r.SFTPGoLogin(context.Background())
	assert.NoError(t, err)
	assert.Contains(t, r.SFTPGoSessionCookie, "jwt=")
	assert.Contains(t, r.SFTPGoSessionCookie, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJXZWJDbGllbnQifQ")
}

func TestSFTPGoLogin_MissingFormToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/login" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`<html><body>No form token here</body></html>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true
	r.SFTPGoUsername = "gpn"
	r.SFTPGoPassword = "gpn"

	err := r.SFTPGoLogin(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "_form_token not found")
}

func TestGetSFTPGoLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/dirs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"name": "file1.zip", "type": "2", "url": "/web/client/files?path=%2Ffile1.zip&_=1"},
				{"name": "subdir", "type": "1", "url": "/web/client/files?path=%2Fsubdir&_=1"}
			]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true

	files, dirs, err := r.getSFTPGoLinks(context.Background(), base.String())
	assert.NoError(t, err)
	assert.Len(t, files, 1)
	assert.Len(t, dirs, 1)
	assert.Contains(t, files[0], "file1.zip")
	assert.Contains(t, dirs[0], "subdir")
}

func TestGetSFTPGoLinks_EmptyDir(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/dirs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true

	files, dirs, err := r.getSFTPGoLinks(context.Background(), base.String())
	assert.NoError(t, err)
	assert.Empty(t, files)
	assert.Empty(t, dirs)
}

func TestExtractCookieValue(t *testing.T) {
	assert.Equal(t, "jwt=abc123", extractCookieValue("jwt=abc123; Path=/", "jwt"))
	assert.Equal(t, "jwt=abc123", extractCookieValue("other=x; jwt=abc123; Path=/", "jwt"))
	assert.Equal(t, "", extractCookieValue("other=x", "jwt"))
}

func TestHTTPClientCookie(t *testing.T) {
	var receivedCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewHTTPClient(2, 0)
	c.client = server.Client()
	c.SetCookie("jwt=session123")

	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	resp, err := c.Do(req)
	assert.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "jwt=session123", receivedCookie)
}
