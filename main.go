package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	arg "github.com/alexflint/go-arg"
	raria2 "github.com/denysvitali/raria2/pkg"
	"github.com/sirupsen/logrus"
)

var version = "v0.1.3.7"

var errUserCanceled = errors.New("operation cancelled by user")

type cliOptions struct {
	Output                 string        `arg:"-o" help:"Output directory (defaults to host/path derived from the URL)"`
	DryRun                 bool          `arg:"-d,--dry-run" help:"Dry Run" default:"false"`
	URL                    string        `arg:"positional,required" help:"The URL to crawl"`
	LogLevel               string        `arg:"--log-level" help:"Log level (panic,fatal,error,warn,info,debug,trace)" default:"info"`
	MaxConnectionPerServer int           `arg:"-x,--max-connection-per-server" help:"Parallel connections per download" default:"5"`
	MaxConcurrentDownload  int           `arg:"-j,--max-concurrent-downloads" help:"Maximum concurrent downloads" default:"5"`
	Threads                int           `arg:"-w,--threads" help:"Concurrent crawler threads" default:"5"`
	Aria2SessionSize       int           `arg:"--aria2-session-size" help:"Number of links to send to a single aria2c process before restarting it (0 = unlimited)" default:"100"`
	MaxDepth               int           `arg:"--max-depth" help:"Maximum HTML depth to crawl (-1 for unlimited)" default:"-1"`
	AcceptExtensions       []string      `arg:"--accept,separate" help:"Comma-separated list(s) of file extensions to include (case-insensitive, without dot)"`
	RejectExtensions       []string      `arg:"--reject,separate" help:"Comma-separated list(s) of file extensions to exclude"`
	AcceptFilenames        []string      `arg:"--accept-filename,separate" help:"Comma-separated list(s) of filename globs to include"`
	RejectFilenames        []string      `arg:"--reject-filename,separate" help:"Comma-separated list(s) of filename globs to exclude"`
	CaseInsensitivePaths   bool          `arg:"--case-insensitive-paths" help:"Make path matching case-insensitive"`
	AcceptPaths            []string      `arg:"--accept-path,separate" help:"Path glob or regex (prefix with regex:) to include"`
	RejectPaths            []string      `arg:"--reject-path,separate" help:"Path glob or regex (prefix with regex:) to exclude"`
	VisitedCachePath       string        `arg:"--visited-cache" help:"Optional file to persist visited URLs for resuming crawls"`
	WriteBatch             string        `arg:"--write-batch" help:"Write aria2 input file to disk instead of executing"`
	HTTPTimeout            time.Duration `arg:"--http-timeout" help:"HTTP client timeout" default:"30s"`
	UserAgent              string        `arg:"--user-agent" help:"Custom User-Agent string" default:"raria2/1.0"`
	RateLimit              float64       `arg:"--rate-limit" help:"Rate limit for HTTP requests (requests per second)" default:"0"`
	RespectRobots          bool          `arg:"--respect-robots" help:"Respect robots.txt when crawling" default:"false"`
	FollowExternal         bool          `arg:"--follow-external" help:"Follow links to external hosts (default: only same host)" default:"false"`
	SFTPGoUser             string        `arg:"--sftpgo-user" help:"SFTPGo web client username (default: extracted from URL)"`
	SFTPGoPassword         string        `arg:"--sftpgo-password" help:"SFTPGo web client password (default: extracted from URL)"`
	AcceptMime             []string      `arg:"--accept-mime,separate" help:"Comma-separated list of MIME types to include"`
	RejectMime             []string      `arg:"--reject-mime,separate" help:"Comma-separated list of MIME types to exclude"`
	Aria2Args              []string      `arg:"positional" help:"Options forwarded to aria2c after the URL (use -- before them if they look like flags)"`
}

// Version implements arg.Versioned for --version support.
func (cliOptions) Version() string {
	return version
}

func main() {
	var opts cliOptions
	arg.MustParse(&opts)
	if err := run(&opts); err != nil {
		if errors.Is(err, errUserCanceled) {
			os.Exit(130)
		}
		logrus.Fatal(err)
	}
}

func run(opts *cliOptions) error {
	level, err := logrus.ParseLevel(opts.LogLevel)
	if err != nil {
		return fmt.Errorf("invalid log level %q: %w", opts.LogLevel, err)
	}
	logrus.SetLevel(level)

	parsedURL, err := url.Parse(opts.URL)
	if err != nil {
		return fmt.Errorf("invalid URL provided: %w", err)
	}
	scheme := strings.ToLower(parsedURL.Scheme)
	if scheme != "http" && scheme != "https" && scheme != "ftp" && scheme != "ftps" {
		return fmt.Errorf("unsupported URL scheme %q (expected http, https, ftp, or ftps)", parsedURL.Scheme)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		sig := <-sigChan
		logrus.Infof("Received signal %v, shutting down gracefully...", sig)
		cancel()
	}()

	// Extract credentials from URL and strip them so they don't leak into requests.
	urlUser := ""
	urlPass := ""
	if parsedURL.User != nil {
		urlUser = parsedURL.User.Username()
		urlPass, _ = parsedURL.User.Password()
		parsedURL.User = nil
	}

	client := raria2.New(parsedURL)
	client.SFTPGoUsername = firstNonEmpty(opts.SFTPGoUser, urlUser)
	client.SFTPGoPassword = firstNonEmpty(opts.SFTPGoPassword, urlPass)
	client.OutputPath = opts.Output
	client.Aria2AfterURLArgs = opts.Aria2Args
	client.DryRun = opts.DryRun
	client.HTTPTimeout = opts.HTTPTimeout
	if strings.ContainsAny(opts.UserAgent, "\r\n") {
		return fmt.Errorf("invalid user-agent: contains newline characters")
	}
	client.UserAgent = opts.UserAgent
	client.RateLimit = opts.RateLimit
	client.MaxDepth = opts.MaxDepth
	client.RespectRobots = opts.RespectRobots
	client.FollowExternal = opts.FollowExternal
	client.VisitedCachePath = opts.VisitedCachePath
	client.WriteBatch = opts.WriteBatch
	client.SkipCertificateCheck = hasAria2CheckCertificateDisabled(opts.Aria2Args)

	if err := setPositiveValue("--max-connection-per-server", opts.MaxConnectionPerServer); err != nil {
		return err
	}
	client.MaxConnectionPerServer = opts.MaxConnectionPerServer

	if err := setPositiveValue("--max-concurrent-downloads", opts.MaxConcurrentDownload); err != nil {
		return err
	}
	client.MaxConcurrentDownload = opts.MaxConcurrentDownload

	if err := setPositiveValue("--threads", opts.Threads); err != nil {
		return err
	}
	client.Threads = opts.Threads
	client.Aria2EntriesPerSession = opts.Aria2SessionSize

	filters := client.FiltersConfig()
	filters.AcceptMime = parseMimeArgs(opts.AcceptMime)
	filters.RejectMime = parseMimeArgs(opts.RejectMime)
	filters.AcceptExtensions = parseExtensionArgs(opts.AcceptExtensions)
	filters.RejectExtensions = parseExtensionArgs(opts.RejectExtensions)
	filters.AcceptFilenames = parseGlobArgs(opts.AcceptFilenames)
	filters.RejectFilenames = parseGlobArgs(opts.RejectFilenames)
	filters.CaseInsensitivePaths = opts.CaseInsensitivePaths

	acceptRegex, err := compilePathPatterns(opts.AcceptPaths)
	if err != nil {
		return fmt.Errorf("invalid --accept-path pattern: %w", err)
	}
	rejectRegex, err := compilePathPatterns(opts.RejectPaths)
	if err != nil {
		return fmt.Errorf("invalid --reject-path pattern: %w", err)
	}
	filters.AcceptPathRegex = acceptRegex
	filters.RejectPathRegex = rejectRegex

	if err := client.RunWithContext(ctx); err != nil {
		if ctx.Err() == context.Canceled {
			logrus.Info("Operation cancelled by user")
			return errUserCanceled
		}
		return err
	}

	return nil
}

func setPositiveValue(name string, value int) error {
	if value < 1 {
		return fmt.Errorf("invalid value for %s: %d", name, value)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseExtensionArgs(values []string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, value := range splitAndTrim(values) {
		value = strings.TrimPrefix(value, ".")
		if value != "" {
			set[strings.ToLower(value)] = struct{}{}
		}
	}
	return set
}

func parseMimeArgs(values []string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, value := range splitAndTrim(values) {
		value = strings.TrimSpace(value)
		if value != "" {
			set[strings.ToLower(value)] = struct{}{}
		}
	}
	return set
}

func parseGlobArgs(values []string) map[string]*regexp.Regexp {
	patterns := make(map[string]*regexp.Regexp)
	for _, value := range splitAndTrim(values) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		re, err := compilePathPattern("glob:" + value)
		if err != nil {
			logrus.Warnf("invalid filename glob pattern %q: %v", value, err)
			continue
		}
		patterns[value] = re
	}
	if len(patterns) == 0 {
		return nil
	}
	return patterns
}

func compilePathPatterns(patterns []string) ([]*regexp.Regexp, error) {
	var compiled []*regexp.Regexp
	for _, raw := range splitAndTrim(patterns) {
		if raw == "" {
			continue
		}
		re, err := compilePathPattern(raw)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

func compilePathPattern(pattern string) (*regexp.Regexp, error) {
	const regexPrefix = "regex:"
	const globPrefix = "glob:"
	switch {
	case strings.HasPrefix(pattern, regexPrefix):
		return regexp.Compile(pattern[len(regexPrefix):])
	case strings.HasPrefix(pattern, globPrefix):
		pattern = pattern[len(globPrefix):]
	}
	return regexp.Compile(globToRegex(pattern))
}

func globToRegex(glob string) string {
	var b strings.Builder
	b.WriteString("^")

	for i := 0; i < len(glob); {
		ch := glob[i]
		switch ch {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				b.WriteString(".*")
				i += 2
				continue
			}
			b.WriteString("[^/]*")
			i++
			continue
		case '?':
			b.WriteString("[^/]")
			i++
			continue
		case '/':
			b.WriteByte('/')
			i++
			continue
		default:
			switch ch {
			case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
				b.WriteByte('\\')
			}
			b.WriteByte(ch)
			i++
		}
	}

	b.WriteString("$")
	return b.String()
}

func splitAndTrim(values []string) []string {
	var result []string
	for _, value := range values {
		parts := strings.Split(value, ",")
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
	}
	return result
}

func hasAria2CheckCertificateDisabled(aria2Args []string) bool {
	for i := 0; i < len(aria2Args); i++ {
		arg := strings.TrimSpace(aria2Args[i])
		if arg == "" {
			continue
		}

		lowerArg := strings.ToLower(arg)
		if lowerArg == "--check-certificate=false" {
			return true
		}
		if lowerArg == "--check-certificate" && i+1 < len(aria2Args) {
			next := strings.ToLower(strings.TrimSpace(aria2Args[i+1]))
			if next == "false" {
				return true
			}
		}
	}
	return false
}
