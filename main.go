package main

import (
	"context"
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

var version = "dev"

var args struct {
	Output                 string        `arg:"-o" help:"Output directory (defaults to host/path derived from the URL)"`
	DryRun                 bool          `arg:"-d,--dry-run" help:"Dry Run" default:"false"`
	Url                    string        `arg:"positional" help:"The URL from where to fetch the resources from"`
	MaxConnectionPerServer int           `arg:"-x,--max-connection-per-server" help:"Parallel connections per download" default:"5"`
	MaxConcurrentDownload  int           `arg:"-j,--max-concurrent-downloads" help:"Maximum concurrent downloads" default:"5"`
	MaxDepth               int           `arg:"--max-depth" help:"Maximum HTML depth to crawl (-1 for unlimited)" default:"-1"`
	AcceptExtensions       []string      `arg:"--accept" help:"Comma-separated list(s) of file extensions to include (case-insensitive, without dot)"`
	RejectExtensions       []string      `arg:"--reject" help:"Comma-separated list(s) of file extensions to exclude"`
	AcceptFilenames        []string      `arg:"--accept-filename" help:"Comma-separated list(s) of filename globs to include"`
	RejectFilenames        []string      `arg:"--reject-filename" help:"Comma-separated list(s) of filename globs to exclude"`
	CaseInsensitivePaths   bool          `arg:"--case-insensitive-paths" help:"Make path matching case-insensitive"`
	AcceptPaths            []string      `arg:"--accept-path" help:"Path glob or regex (prefix with regex:) to include"`
	RejectPaths            []string      `arg:"--reject-path" help:"Path glob or regex (prefix with regex:) to exclude"`
	VisitedCachePath       string        `arg:"--visited-cache" help:"Optional file to persist visited URLs for resuming crawls"`
	WriteBatch             string        `arg:"--write-batch" help:"Write aria2 input file to disk instead of executing"`
	HTTPTimeout            time.Duration `arg:"--http-timeout" help:"HTTP client timeout" default:"30s"`
	UserAgent              string        `arg:"--user-agent" help:"Custom User-Agent string" default:"raria2/1.0"`
	RateLimit              float64       `arg:"--rate-limit" help:"Rate limit for HTTP requests (requests per second)" default:"0"`
	Aria2Args              []string      `arg:"positional" help:"Options forwarded to aria2c after the URL (use -- before them if they look like flags)"`
}

func main() {
	arg.MustParse(&args)

	if args.Url == "" {
		logrus.Fatalf("please provide an URL (version: %s)", version)
	}

	// Set up context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		logrus.Infof("Received signal %v, shutting down gracefully...", sig)
		cancel()
	}()

	parsedUrl, err := url.Parse(args.Url)
	if err != nil {
		logrus.Fatalf("invalid URL provided")
	}

	client := raria2.New(parsedUrl)
	client.OutputPath = args.Output
	client.Aria2AfterURLArgs = args.Aria2Args
	client.DryRun = args.DryRun
	client.HTTPTimeout = args.HTTPTimeout
	client.UserAgent = args.UserAgent
	client.RateLimit = args.RateLimit
	client.MaxDepth = args.MaxDepth
	client.AcceptExtensions = parseExtensionArgs(args.AcceptExtensions)
	client.RejectExtensions = parseExtensionArgs(args.RejectExtensions)
	client.AcceptFilenames = parseGlobArgs(args.AcceptFilenames)
	client.RejectFilenames = parseGlobArgs(args.RejectFilenames)
	client.CaseInsensitivePaths = args.CaseInsensitivePaths
	client.VisitedCachePath = args.VisitedCachePath
	client.WriteBatch = args.WriteBatch
	acceptRegex, err := compilePathPatterns(args.AcceptPaths)
	if err != nil {
		logrus.Fatalf("invalid --accept-path pattern: %v", err)
	}
	rejectRegex, err := compilePathPatterns(args.RejectPaths)
	if err != nil {
		logrus.Fatalf("invalid --reject-path pattern: %v", err)
	}
	client.AcceptPathRegex = acceptRegex
	client.RejectPathRegex = rejectRegex

	if args.MaxConnectionPerServer < 1 {
		logrus.Fatalf("invalid value for --max-connection-per-server: %d", args.MaxConnectionPerServer)
	}
	client.MaxConnectionPerServer = args.MaxConnectionPerServer

	if args.MaxConcurrentDownload < 1 {
		logrus.Fatalf("invalid value for --max-concurrent-downloads: %d", args.MaxConcurrentDownload)
	}
	client.MaxConcurrentDownload = args.MaxConcurrentDownload

	err = client.RunWithContext(ctx)
	if err != nil {
		if ctx.Err() == context.Canceled {
			logrus.Info("Operation cancelled by user")
			os.Exit(130) // Standard exit code for SIGINT
		}
		logrus.Fatal(err)
	}
}

func parseExtensionArgs(values []string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, value := range splitAndTrim(values) {
		value = strings.TrimPrefix(value, ".")
		value = strings.ToLower(value)
		if value == "" {
			continue
		}
		set[value] = struct{}{}
	}
	if len(set) == 0 {
		return nil
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
		// Convert glob pattern to regex
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

	// Handle ** specially (matches across directories)
	if strings.Contains(glob, "**") {
		return globToRegexAdvanced(glob)
	}

	// Simple glob: * matches within one segment (no /)
	segments := strings.Split(glob, "/")
	for i, segment := range segments {
		if i > 0 {
			b.WriteString("/")
		}
		if segment == "" {
			continue // Handle leading/trailing slashes
		}

		// Convert * to [^/]* (match anything except / within this segment)
		regexSegment := strings.ReplaceAll(regexp.QuoteMeta(segment), "*", "[^/]*")
		b.WriteString(regexSegment)
	}

	b.WriteString("$")
	return b.String()
}

func globToRegexAdvanced(glob string) string {
	var b strings.Builder
	b.WriteString("^")

	// Advanced glob with ** support
	segments := strings.Split(glob, "/")
	for i, segment := range segments {
		if i > 0 {
			b.WriteString("/")
		}
		if segment == "" {
			continue // Handle leading/trailing slashes
		}

		if segment == "**" {
			// ** matches zero or more path segments
			b.WriteString("(?:[^/]+/)*[^/]*")
		} else {
			// Convert * to [^/]* (match anything except / within this segment)
			regexSegment := strings.ReplaceAll(regexp.QuoteMeta(segment), "*", "[^/]*")
			b.WriteString(regexSegment)
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
