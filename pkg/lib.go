package raria2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
)

var (
	execCommand = exec.Command
	lookPath    = exec.LookPath
)

type RAria2 struct {
	url                    *url.URL
	MaxConnectionPerServer int
	MaxConcurrentDownload  int
	MaxDepth               int
	OutputPath             string
	Aria2AfterURLArgs      []string
	HTTPTimeout            time.Duration
	UserAgent              string
	RateLimit              float64
	VisitedCachePath       string
	AcceptExtensions       map[string]struct{}
	RejectExtensions       map[string]struct{}
	AcceptFilenames        map[string]*regexp.Regexp
	RejectFilenames        map[string]*regexp.Regexp
	CaseInsensitivePaths   bool
	AcceptPathRegex        []*regexp.Regexp
	RejectPathRegex        []*regexp.Regexp
	WriteBatch             string
	httpClient             *http.Client

	downloadEntries   []aria2URLEntry
	downloadEntriesMu sync.Mutex

	lastRequest int64 // Unix timestamp with nanoseconds

	// Disable retries (useful for testing)
	DisableRetries bool

	// does not perform any resource download
	DryRun bool

	urlCache   map[string]struct{}
	urlCacheMu sync.Mutex
}

func New(url *url.URL) *RAria2 {
	return &RAria2{
		url:                    url,
		MaxConnectionPerServer: 5,
		MaxConcurrentDownload:  5,
		MaxDepth:               -1,
		HTTPTimeout:            30 * time.Second,
		urlCache:               make(map[string]struct{}),
	}
}

func (r *RAria2) ensureOutputPath() error {
	if r.OutputPath == "" {
		if r.url == nil {
			return fmt.Errorf("unable to derive output path: source URL is nil")
		}
		host := r.url.Host
		path := strings.Trim(r.url.Path, "/")
		if host == "" {
			host = "download"
		}
		if path == "" {
			r.OutputPath = host
		} else {
			r.OutputPath = filepath.Join(host, filepath.FromSlash(path))
		}
	}
	if _, err := os.Stat(r.OutputPath); os.IsNotExist(err) {
		if err := os.MkdirAll(r.OutputPath, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (r *RAria2) client() *http.Client {
	if r.httpClient != nil {
		return r.httpClient
	}

	timeout := r.HTTPTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	r.httpClient = &http.Client{Timeout: timeout}
	return r.httpClient
}

func (r *RAria2) waitForRateLimit() {
	if r.RateLimit <= 0 {
		return
	}

	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&r.lastRequest)

	// Calculate minimum time between requests
	minInterval := time.Second / time.Duration(r.RateLimit)

	if now-last < int64(minInterval) {
		sleepTime := time.Duration(minInterval - time.Duration(now-last))
		time.Sleep(sleepTime)
	}

	atomic.StoreInt64(&r.lastRequest, time.Now().UnixNano())
}

func (r *RAria2) doHTTPRequestWithRetry(req *http.Request) (*http.Response, error) {
	// If retries are disabled, just do a single request
	if r.DisableRetries {
		r.waitForRateLimit()
		return r.client().Do(req)
	}

	const maxRetries = 3
	const baseDelay = 100 * time.Millisecond

	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			delay := baseDelay * time.Duration(1<<uint(attempt-1))
			jitter := time.Duration(float64(delay) * 0.1 * (0.5 + 0.5*rand.Float64()))
			time.Sleep(delay + jitter)

			logrus.Debugf("Retrying HTTP request (attempt %d/%d)", attempt+1, maxRetries)
		}

		r.waitForRateLimit()

		resp, err := r.client().Do(req)
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
		if isTransientError(err) {
			lastErr = err
			continue
		}

		// Non-transient error, return immediately
		return nil, err
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func isTransientError(err error) bool {
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

func (r *RAria2) IsHtmlPage(urlString string) (bool, error) {
	// First try HEAD request
	req, err := http.NewRequest("HEAD", urlString, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", r.UserAgent)

	res, err := r.doHTTPRequestWithRetry(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()

	// If HEAD fails with 405/403 or missing Content-Type, fall back to GET
	if res.StatusCode == 405 || res.StatusCode == 403 ||
		res.Header.Get("Content-Type") == "" {

		// Try GET with Range header first for efficiency
		req, err = http.NewRequest("GET", urlString, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("User-Agent", r.UserAgent)
		req.Header.Set("Range", "bytes=0-1023")
		res, err = r.doHTTPRequestWithRetry(req)
		if err != nil {
			return false, err
		}
		defer res.Body.Close()

		// If Range not supported, read first 1KB normally
		if res.StatusCode == 416 || res.StatusCode == 400 {
			req, err = http.NewRequest("GET", urlString, nil)
			if err != nil {
				return false, err
			}
			req.Header.Set("User-Agent", r.UserAgent)
			res, err = r.doHTTPRequestWithRetry(req)
			if err != nil {
				return false, err
			}
			defer res.Body.Close()
		}

		// For successful GET (either Range or full), read first 1KB to detect content type
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			limitReader := io.LimitReader(res.Body, 1024)
			bodyBytes, _ := io.ReadAll(limitReader)
			contentType := http.DetectContentType(bodyBytes)
			return isHTMLContent(contentType), nil
		}
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false, fmt.Errorf("unexpected status code %d for %s", res.StatusCode, urlString)
	}

	return isHTMLContent(res.Header.Get("Content-Type")), nil
}

var errNotHTML = errors.New("content is not HTML")

func (r *RAria2) getLinksByUrl(urlString string) ([]string, error) {
	return r.getLinksByUrlWithContext(context.Background(), urlString)
}

func (r *RAria2) getLinksByUrlWithContext(ctx context.Context, urlString string) ([]string, error) {
	parsedUrl, err := url.Parse(urlString)
	if err != nil {
		return []string{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", urlString, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.UserAgent)

	res, err := r.doHTTPRequestWithRetry(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status code %d for %s", res.StatusCode, urlString)
	}

	if !isHTMLContent(res.Header.Get("Content-Type")) {
		return nil, errNotHTML
	}

	return getLinks(parsedUrl, res.Body)
}

func (r *RAria2) Run() error {
	return r.RunWithContext(context.Background())
}

func (r *RAria2) RunWithContext(ctx context.Context) error {
	if _, err := lookPath("aria2c"); err != nil {
		return fmt.Errorf("aria2c is required but was not found in PATH: %w", err)
	}

	if err := r.loadVisitedCache(); err != nil {
		return fmt.Errorf("failed loading visited cache: %w", err)
	}

	if err := r.ensureOutputPath(); err != nil {
		return err
	}
	dir, _ := os.Getwd()
	logrus.Infof("pwd: %v", dir)

	r.subDownloadUrls(ctx, 0, r.url.String())

	if err := r.saveVisitedCache(); err != nil {
		return fmt.Errorf("failed saving visited cache: %w", err)
	}

	return r.executeBatchDownload()
}

func (r *RAria2) subDownloadUrls(ctx context.Context, workerId int, startURL string) {
	type crawlEntry struct {
		url   string
		depth int
	}

	queue := []crawlEntry{{url: startURL, depth: 0}}

	for len(queue) > 0 {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			logrus.Info("Crawling cancelled by context")
			return
		default:
		}

		entry := queue[0]
		queue = queue[1:]
		cUrl := entry.url
		parsedURL, err := url.Parse(cUrl)
		if err != nil {
			logrus.Warnf("skipping invalid URL %s: %v", cUrl, err)
			continue
		}

		if !r.pathAllowed(parsedURL) {
			logrus.Debugf("path filters skipped %s", cUrl)
			continue
		}

		if !r.markVisited(cUrl) {
			logrus.WithField("url", cUrl).Debug("skipping already visited")
			continue
		}

		newLinks, err := r.getLinksByUrlWithContext(ctx, cUrl)
		if err != nil {
			if errors.Is(err, errNotHTML) {
				r.downloadResource(workerId, cUrl)
				continue
			}
			logrus.Warnf("unable to fetch %v: %v", cUrl, err)
			continue
		}

		nextDepth := entry.depth + 1
		if r.MaxDepth >= 0 && nextDepth > r.MaxDepth {
			continue
		}

		for _, link := range newLinks {
			parsedLink, err := url.Parse(link)
			if err != nil {
				logrus.Debugf("skipping invalid discovered URL %s: %v", link, err)
				continue
			}
			if !r.pathAllowed(parsedLink) {
				logrus.Debugf("path filters skipped %s", link)
				continue
			}
			queue = append(queue, crawlEntry{url: link, depth: nextDepth})
		}
	}
}

func (r *RAria2) downloadResource(workerId int, cUrl string) {
	parsedCUrl, err := url.Parse(cUrl)
	if err != nil {
		logrus.Warnf("[W %d]: unable to download %v because it's an invalid URL: %v", workerId, cUrl, err)
		return
	}

	if !r.pathAllowed(parsedCUrl) {
		logrus.Debugf("[W %d]: skipping %s due to path filters", workerId, cUrl)
		return
	}

	if !r.extensionAllowed(parsedCUrl) {
		logrus.Debugf("[W %d]: skipping %s due to extension filters", workerId, cUrl)
		return
	}

	if !r.filenameAllowed(parsedCUrl) {
		logrus.Debugf("[W %d]: skipping %s due to filename filters", workerId, cUrl)
		return
	}

	// Get relative directory
	var outputPath string
	p1 := parsedCUrl.Path
	p2 := r.url.Path

	idx := strings.Index(p1, p2)
	if idx == 0 {
		outputPath = strings.TrimPrefix(p1, p2)
	} else {
		outputPath = parsedCUrl.Host + "/" + parsedCUrl.Path
	}

	if r.DryRun {
		logrus.Infof("[W %d]: dry run: downloading %s to %s", workerId, cUrl, outputPath)
	}

	outputDir := filepath.Join(r.OutputPath, filepath.Dir(outputPath))
	if err := r.ensureOutputDir(workerId, outputDir); err != nil {
		logrus.Warnf("unable to create %v: %v", outputDir, err)
		return
	}

	entry := aria2URLEntry{URL: cUrl, Dir: outputDir}
	r.enqueueDownloadEntry(entry)
	logrus.Infof("[W %d]: queued %s for batch download (dir=%s)", workerId, cUrl, outputDir)
}

func (r *RAria2) markVisited(u string) bool {
	key := canonicalCacheKey(u)
	r.urlCacheMu.Lock()
	defer r.urlCacheMu.Unlock()
	if _, exists := r.urlCache[key]; exists {
		return false
	}
	r.urlCache[key] = struct{}{}
	return true
}

func (r *RAria2) loadVisitedCache() error {
	if r.VisitedCachePath == "" {
		return nil
	}

	file, err := os.Open(r.VisitedCachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var entries []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	r.urlCacheMu.Lock()
	for _, entry := range entries {
		r.urlCache[canonicalCacheKey(entry)] = struct{}{}
	}
	r.urlCacheMu.Unlock()

	return nil
}

func (r *RAria2) saveVisitedCache() error {
	if r.VisitedCachePath == "" {
		return nil
	}

	r.urlCacheMu.Lock()
	keys := make([]string, 0, len(r.urlCache))
	for k := range r.urlCache {
		keys = append(keys, k)
	}
	r.urlCacheMu.Unlock()
	sort.Strings(keys)

	dir := filepath.Dir(r.VisitedCachePath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmpPath := r.VisitedCachePath + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(file)
	for _, key := range keys {
		if _, err := fmt.Fprintln(writer, key); err != nil {
			file.Close()
			_ = os.Remove(tmpPath)
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, r.VisitedCachePath)
}

// canonicalURL returns a normalized version of a URL for consistent comparison
// This function should be used everywhere URL comparison is needed
func canonicalURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	// Normalize scheme to lowercase
	parsed.Scheme = strings.ToLower(parsed.Scheme)

	// Normalize host to lowercase
	if parsed.Host != "" {
		parsed.Host = strings.ToLower(parsed.Host)

		// Normalize default ports
		switch parsed.Scheme {
		case "http":
			if parsed.Port() == "80" {
				parsed.Host = parsed.Hostname()
			}
		case "https":
			if parsed.Port() == "443" {
				parsed.Host = parsed.Hostname()
			}
		}
	}

	// Always strip fragments
	parsed.Fragment = ""

	// Normalize path - but preserve root path
	if parsed.Path == "" {
		parsed.Path = "/"
	} else if parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/") {
		// Remove trailing slashes for consistency (except root)
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}

	// For directory-style paths (ending with /) or root paths with query params,
	// clear query params (handles server directory listing sorting parameters)
	if strings.HasSuffix(raw, "/") || (parsed.Path == "/" && parsed.RawQuery != "") {
		parsed.RawQuery = ""
	}

	return parsed.String()
}

func canonicalCacheKey(raw string) string {
	return canonicalURL(raw)
}

func (r *RAria2) pathAllowed(u *url.URL) bool {
	pathStr := u.Path
	if pathStr == "" {
		pathStr = "/"
	}

	// Apply case-insensitive matching if enabled
	if r.CaseInsensitivePaths {
		pathStr = strings.ToLower(pathStr)
	}

	// For case-insensitive matching, we need to use case-insensitive regex patterns
	var rejectPatterns []*regexp.Regexp
	var acceptPatterns []*regexp.Regexp

	if r.CaseInsensitivePaths {
		// Convert all patterns to case-insensitive
		for _, re := range r.RejectPathRegex {
			caseInsensitiveRe, err := regexp.Compile("(?i)" + re.String())
			if err == nil {
				rejectPatterns = append(rejectPatterns, caseInsensitiveRe)
			} else {
				rejectPatterns = append(rejectPatterns, re) // fallback
			}
		}
		for _, re := range r.AcceptPathRegex {
			caseInsensitiveRe, err := regexp.Compile("(?i)" + re.String())
			if err == nil {
				acceptPatterns = append(acceptPatterns, caseInsensitiveRe)
			} else {
				acceptPatterns = append(acceptPatterns, re) // fallback
			}
		}
	} else {
		rejectPatterns = r.RejectPathRegex
		acceptPatterns = r.AcceptPathRegex
	}

	if matchAnyRegex(rejectPatterns, pathStr) {
		return false
	}

	if len(acceptPatterns) == 0 {
		return true
	}

	if matchAnyRegex(acceptPatterns, pathStr) {
		return true
	}

	basePath := r.url.Path
	if basePath == "" {
		basePath = "/"
	}

	// Apply case-insensitive matching to base path if enabled
	if r.CaseInsensitivePaths {
		basePath = strings.ToLower(basePath)
	}

	trimPath := strings.TrimSuffix(pathStr, "/")
	trimBase := strings.TrimSuffix(basePath, "/")
	if trimPath == "" {
		trimPath = "/"
	}
	if trimBase == "" {
		trimBase = "/"
	}

	return trimPath == trimBase
}

func (r *RAria2) extensionAllowed(u *url.URL) bool {
	if len(r.AcceptExtensions) == 0 && len(r.RejectExtensions) == 0 {
		return true
	}

	ext := strings.ToLower(strings.TrimPrefix(path.Ext(u.Path), "."))

	if len(r.AcceptExtensions) > 0 {
		if _, ok := r.AcceptExtensions[ext]; !ok {
			return false
		}
	}

	if len(r.RejectExtensions) > 0 {
		if _, ok := r.RejectExtensions[ext]; ok {
			return false
		}
	}

	return true
}

func (r *RAria2) filenameAllowed(u *url.URL) bool {
	if len(r.AcceptFilenames) == 0 && len(r.RejectFilenames) == 0 {
		return true
	}

	filename := path.Base(u.Path)

	if len(r.AcceptFilenames) > 0 {
		accepted := false
		for _, pattern := range r.AcceptFilenames {
			if pattern.MatchString(filename) {
				accepted = true
				break
			}
		}
		if !accepted {
			return false
		}
	}

	if len(r.RejectFilenames) > 0 {
		for _, pattern := range r.RejectFilenames {
			if pattern.MatchString(filename) {
				return false
			}
		}
	}

	return true
}

func matchAnyRegex(patterns []*regexp.Regexp, value string) bool {
	for _, re := range patterns {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

func (r *RAria2) ensureOutputDir(workerId int, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if r.DryRun {
			logrus.Infof("[W %d]: dry run: creating folder %s", workerId, dir)
			return nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (r *RAria2) enqueueDownloadEntry(entry aria2URLEntry) {
	r.downloadEntriesMu.Lock()
	defer r.downloadEntriesMu.Unlock()
	r.downloadEntries = append(r.downloadEntries, entry)
}

func (r *RAria2) executeBatchDownload() error {
	r.downloadEntriesMu.Lock()
	entries := make([]aria2URLEntry, len(r.downloadEntries))
	copy(entries, r.downloadEntries)
	r.downloadEntriesMu.Unlock()

	if len(entries) == 0 {
		logrus.Info("no downloadable resources found")
		return nil
	}

	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)
	for _, entry := range entries {
		if _, err := fmt.Fprintln(writer, entry.URL); err != nil {
			return err
		}
		if entry.Dir != "" {
			if _, err := fmt.Fprintf(writer, "  dir=%s\n", entry.Dir); err != nil {
				return err
			}
		}
	}

	if err := writer.Flush(); err != nil {
		return err
	}

	// If WriteBatch is set, write to file instead of executing aria2c
	if r.WriteBatch != "" {
		return r.writeBatchFile(buf.Bytes())
	}

	binFile := "aria2c"
	args := []string{"-x", strconv.Itoa(r.MaxConnectionPerServer)}
	if r.MaxConcurrentDownload > 0 {
		args = append(args, "-j", strconv.Itoa(r.MaxConcurrentDownload))
	}
	args = append(args, "--input-file", "-", "--deferred-input=true")

	if r.DryRun {
		args = append(args, "--dry-run=true")
	}

	if len(r.Aria2AfterURLArgs) > 0 {
		args = append(args, r.Aria2AfterURLArgs...)
	}

	if r.DryRun {
		logrus.Infof("aria2 batch cmd: %s %s", binFile, strings.Join(args, " "))
		return nil
	}

	cmd := execCommand(binFile, args...)
	cmd.Stdin = bytes.NewReader(buf.Bytes())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aria2 batch command failed: %w", err)
	}

	return nil
}

func (r *RAria2) writeBatchFile(content []byte) error {
	// Create the file
	file, err := os.Create(r.WriteBatch)
	if err != nil {
		return fmt.Errorf("failed to create batch file %s: %w", r.WriteBatch, err)
	}
	defer file.Close()

	// Write the content
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("failed to write batch file %s: %w", r.WriteBatch, err)
	}

	logrus.Infof("wrote aria2 batch file with %d download entries to %s",
		len(r.downloadEntries), r.WriteBatch)
	return nil
}

type aria2URLEntry struct {
	URL string
	Dir string
}

func getLinks(originalUrl *url.URL, body io.ReadCloser) ([]string, error) {
	document, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return []string{}, err
	}

	var urlList []string

	document.Find("a[href]").Each(func(i int, selection *goquery.Selection) {
		val, exists := selection.Attr("href")
		if !exists {
			return
		}

		aHrefUrl, err := url.Parse(val)
		if err != nil {
			logrus.Infof("skipping %v because it is not a valid URL", val)
			return
		}

		resolvedUrl := originalUrl.ResolveReference(aHrefUrl)

		if SameUrl(resolvedUrl, originalUrl) {
			return
		}

		if IsSubPath(resolvedUrl, originalUrl) {
			urlList = append(urlList, resolvedUrl.String())
		}
	})

	return urlList, nil
}

func isHTMLContent(contentType string) bool {
	contentTypeEntries := strings.Split(contentType, ";")
	for _, v := range contentTypeEntries {
		if strings.TrimSpace(v) == "text/html" {
			return true
		}
	}
	return false
}

func IsSubPath(subject *url.URL, of *url.URL) bool {
	// Use canonical URLs for consistent comparison
	subjectCanonical := canonicalURL(subject.String())
	ofCanonical := canonicalURL(of.String())

	subjectParsed, _ := url.Parse(subjectCanonical)
	ofParsed, _ := url.Parse(ofCanonical)

	if subjectParsed.Host != ofParsed.Host {
		return false
	}

	if subjectParsed.Scheme != ofParsed.Scheme {
		return false
	}

	basePath := ofParsed.Path
	if basePath == "" {
		basePath = "/"
	}

	baseNoSlash := basePath
	if baseNoSlash != "/" {
		baseNoSlash = strings.TrimSuffix(baseNoSlash, "/")
	}

	baseWithSlash := baseNoSlash
	if baseNoSlash != "/" {
		baseWithSlash = baseNoSlash + "/"
	}

	if subjectParsed.Path == baseNoSlash {
		return true
	}

	return strings.HasPrefix(subjectParsed.Path, baseWithSlash)
}

// SameUrl checks if two URLs are considered the same using canonicalization
func SameUrl(a *url.URL, b *url.URL) bool {
	return canonicalURL(a.String()) == canonicalURL(b.String())
}
