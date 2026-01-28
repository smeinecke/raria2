package raria2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
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
	VisitedCachePath       string
	AcceptExtensions       map[string]struct{}
	RejectExtensions       map[string]struct{}
	AcceptPathRegex        []*regexp.Regexp
	RejectPathRegex        []*regexp.Regexp
	httpClient             *http.Client

	downloadEntries   []aria2URLEntry
	downloadEntriesMu sync.Mutex

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

func (r *RAria2) IsHtmlPage(urlString string) (bool, error) {
	req, err := http.NewRequest("HEAD", urlString, nil)
	if err != nil {
		return false, err
	}

	res, err := r.client().Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false, fmt.Errorf("unexpected status code %d for %s", res.StatusCode, urlString)
	}

	return isHTMLContent(res.Header.Get("Content-Type")), nil
}

var errNotHTML = errors.New("content is not HTML")

func (r *RAria2) getLinksByUrl(urlString string) ([]string, error) {
	parsedUrl, err := url.Parse(urlString)
	if err != nil {
		return []string{}, err
	}

	req, err := http.NewRequest("GET", urlString, nil)
	if err != nil {
		return nil, err
	}

	res, err := r.client().Do(req)
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

	r.subDownloadUrls(0, r.url.String())

	if err := r.saveVisitedCache(); err != nil {
		return fmt.Errorf("failed saving visited cache: %w", err)
	}

	return r.executeBatchDownload()

}

func (r *RAria2) subDownloadUrls(workerId int, startURL string) {
	type crawlEntry struct {
		url   string
		depth int
	}

	queue := []crawlEntry{{url: startURL, depth: 0}}

	for len(queue) > 0 {
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
			logrus.Infof("cache hit for %v. won't re-visit", cUrl)
			continue
		}

		newLinks, err := r.getLinksByUrl(cUrl)
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
		r.urlCache[entry] = struct{}{}
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

func canonicalCacheKey(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}

	if strings.HasSuffix(parsed.Path, "/") {
		parsed.RawQuery = ""
	}

	return parsed.String()
}

func (r *RAria2) pathAllowed(u *url.URL) bool {
	pathStr := u.Path
	if pathStr == "" {
		pathStr = "/"
	}

	if matchAnyRegex(r.RejectPathRegex, pathStr) {
		return false
	}

	if len(r.AcceptPathRegex) == 0 {
		return true
	}

	if matchAnyRegex(r.AcceptPathRegex, pathStr) {
		return true
	}

	basePath := r.url.Path
	if basePath == "" {
		basePath = "/"
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

type aria2URLEntry struct {
	URL string
	Dir string
}

func getLinks(originalUrl *url.URL, body io.ReadCloser) ([]string, error) {
	document, err := goquery.NewDocumentFromReader(body)

	var urlList []string

	if err != nil {
		return urlList, err
	}

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
		resolvedRefUrl := originalUrl.ResolveReference(aHrefUrl)
		if SameUrl(resolvedRefUrl, originalUrl) {
			return
		}

		if IsSubPath(resolvedRefUrl, originalUrl) {
			urlList = append(urlList, resolvedRefUrl.String())
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
	if subject.Host != of.Host {
		return false
	}

	if subject.Scheme != of.Scheme {
		return false
	}

	basePath := of.Path
	if basePath == "" {
		basePath = "/"
	}

	baseNoSlash := basePath
	if baseNoSlash != "/" {
		baseNoSlash = strings.TrimSuffix(baseNoSlash, "/")
	}

	baseWithSlash := baseNoSlash
	if baseWithSlash != "/" {
		baseWithSlash = baseNoSlash + "/"
	}

	if subject.Path == baseNoSlash {
		return true
	}

	return strings.HasPrefix(subject.Path, baseWithSlash)
}

// An URL is considered to be the same in our context
// when they share the same hostname and path.
// In our case, /, /?C=N;O=D, /?C=M;O=A, ... are all considered to be the same URL.
func SameUrl(a *url.URL, b *url.URL) bool {
	if a.Host != b.Host {
		return false
	}

	if a.Path != b.Path {
		return false
	}

	if a.Port() != b.Port() {
		return false
	}

	return true
}
