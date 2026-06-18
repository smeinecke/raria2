package raria2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
)

const sftpgoLoginPath = "/web/client/login"
const sftpgoDirsPath = "/web/client/dirs"

// IsSFTPGoURL checks whether the URL path suggests an SFTPGo web client endpoint.
func IsSFTPGoURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	return strings.Contains(u.Path, "/web/client/")
}

// DetectSFTPGo fetches the SFTPGo login page and validates it by looking for the
// expected version marker in the HTML body.
func (r *RAria2) DetectSFTPGo(ctx context.Context) (bool, error) {
	loginURL := r.sftpgoBaseURL().String() + sftpgoLoginPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", r.UserAgent)

	resp, err := r.client().DoWithRetry(req)
	if err != nil {
		return false, fmt.Errorf("failed to fetch SFTPGo login page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false, fmt.Errorf("failed to read SFTPGo login page: %w", err)
	}

	return strings.Contains(string(body), "'SFTPGo 2."), nil
}

// SFTPGoLogin performs the two-step SFTPGo web-client login and stores the
// resulting session cookie on the RAria2 instance.
func (r *RAria2) SFTPGoLogin(ctx context.Context) error {
	base := r.sftpgoBaseURL()
	loginURL := base.String() + sftpgoLoginPath

	username := r.SFTPGoUsername
	password := r.SFTPGoPassword

	if username == "" || password == "" {
		return fmt.Errorf("SFTPGo credentials not provided")
	}

	// Step 1: GET login page to obtain the initial jwt cookie and _form_token.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", r.UserAgent)

	resp, err := r.client().DoWithRetry(req)
	if err != nil {
		return fmt.Errorf("SFTPGo login GET failed: %w", err)
	}

	var initialCookie string
	for _, c := range resp.Cookies() {
		if c.Name == "jwt" {
			initialCookie = c.String()
			break
		}
	}
	if initialCookie == "" {
		// Fallback: look at Set-Cookie header manually if cookie jar didn't capture it
		setCookie := resp.Header.Get("Set-Cookie")
		if strings.Contains(setCookie, "jwt=") {
			initialCookie = extractCookieValue(setCookie, "jwt")
		}
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("failed to parse SFTPGo login page: %w", err)
	}

	formToken, exists := doc.Find("input[name='_form_token']").Attr("value")
	if !exists || formToken == "" {
		return fmt.Errorf("SFTPGo _form_token not found in login page")
	}

	// Step 2: POST login credentials with the jwt cookie.
	formData := url.Values{}
	formData.Set("username", username)
	formData.Set("password", password)
	formData.Set("_form_token", formToken)

	req, err = http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", r.UserAgent)
	if initialCookie != "" {
		req.Header.Set("Cookie", initialCookie)
	}

	// Use a client that does not follow redirects so we can read the
	// Set-Cookie header on the 302 response directly.
	noRedirectClient := *r.client().client
	noRedirectClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err = noRedirectClient.Do(req)
	if err != nil {
		return fmt.Errorf("SFTPGo login POST failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SFTPGo login POST returned status %d", resp.StatusCode)
	}

	// Extract the session jwt cookie.
	var sessionCookie string
	for _, c := range resp.Cookies() {
		if c.Name == "jwt" {
			sessionCookie = c.String()
			break
		}
	}
	if sessionCookie == "" {
		setCookie := resp.Header.Get("Set-Cookie")
		if strings.Contains(setCookie, "jwt=") {
			sessionCookie = extractCookieValue(setCookie, "jwt")
		}
	}
	if sessionCookie == "" {
		return fmt.Errorf("SFTPGo session cookie not received after login")
	}

	r.SFTPGoSessionCookie = sessionCookie
	r.client().SetCookie(sessionCookie)
	logrus.Infof("SFTPGo login successful, session cookie acquired")
	return nil
}

// sftpgoBaseURL returns the scheme://host portion of the configured URL.
func (r *RAria2) sftpgoBaseURL() *url.URL {
	return &url.URL{
		Scheme: r.url.Scheme,
		Host:   r.url.Host,
	}
}

// getSFTPGoLinks fetches the SFTPGo JSON directory listing and returns
// file URLs (type "2") and directory URLs (type "1").
func (r *RAria2) getSFTPGoLinks(ctx context.Context, urlString string) (files []string, dirs []string, err error) {
	parsed, err := url.Parse(urlString)
	if err != nil {
		return nil, nil, err
	}

	// Build the dirs API URL.
	dirsURL := &url.URL{
		Scheme: parsed.Scheme,
		Host:   parsed.Host,
		Path:   sftpgoDirsPath,
	}

	// Extract the path query parameter from the original URL.
	q := parsed.Query()
	pathParam := q.Get("path")
	if pathParam == "" {
		// Fallback: derive from the URL path after /web/client/files
		if strings.HasPrefix(parsed.Path, "/web/client/files") {
			pathParam = strings.TrimPrefix(parsed.Path, "/web/client/files")
			pathParam = strings.TrimPrefix(pathParam, "/")
		}
	}
	if pathParam == "" {
		pathParam = "/"
	}

	reqQ := dirsURL.Query()
	reqQ.Set("path", pathParam)
	dirsURL.RawQuery = reqQ.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dirsURL.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", r.UserAgent)

	var resp *http.Response
	if r.DisableRetries {
		resp, err = r.client().Do(req)
	} else {
		resp, err = r.client().DoWithRetry(req)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch SFTPGo dirs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("SFTPGo dirs returned status %d", resp.StatusCode)
	}

	var entries []sftpgoDirEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, nil, fmt.Errorf("failed to decode SFTPGo dirs JSON: %w", err)
	}

	base := r.sftpgoBaseURL()
	for _, e := range entries {
		entryURL := e.URL
		if !strings.HasPrefix(entryURL, "http") {
			// Relative URL, resolve against base
			entryURL = base.String() + entryURL
		}
		switch e.Type {
		case "1":
			dirs = append(dirs, entryURL)
		case "2":
			files = append(files, entryURL)
		default:
			// Treat unknown as file for safety
			files = append(files, entryURL)
		}
	}

	return files, dirs, nil
}

type sftpgoDirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

// extractCookieValue pulls a specific cookie value out of a Set-Cookie or Cookie header string.
func extractCookieValue(header, name string) string {
	// Simple regex-based extraction for the first matching cookie.
	re := regexp.MustCompile(`(?:^|;\s*)` + regexp.QuoteMeta(name) + `=([^;]+)`)
	m := re.FindStringSubmatch(header)
	if len(m) > 1 {
		return name + "=" + m[1]
	}
	return ""
}
