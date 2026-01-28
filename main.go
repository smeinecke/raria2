package main

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	arg "github.com/alexflint/go-arg"
	raria2 "github.com/denysvitali/raria2/pkg"
	"github.com/sirupsen/logrus"
)

var args struct {
	Output                 string        `arg:"-o" help:"Output directory (defaults to host/path derived from the URL)"`
	DryRun                 bool          `arg:"-d,--dry-run" help:"Dry Run" default:"false"`
	Url                    string        `arg:"positional" help:"The URL from where to fetch the resources from"`
	MaxConnectionPerServer int           `arg:"-x,--max-connection-per-server" help:"Parallel connections per download" default:"5"`
	MaxConcurrentDownload  int           `arg:"-j,--max-concurrent-downloads" help:"Maximum concurrent downloads" default:"5"`
	MaxDepth               int           `arg:"--max-depth" help:"Maximum HTML depth to crawl (-1 for unlimited)" default:"-1"`
	AcceptExtensions       []string      `arg:"--accept" help:"Comma-separated list(s) of file extensions to include (case-insensitive, without dot)"`
	RejectExtensions       []string      `arg:"--reject" help:"Comma-separated list(s) of file extensions to exclude"`
	AcceptPaths            []string      `arg:"--accept-path" help:"Path glob or regex (prefix with regex:) to include"`
	RejectPaths            []string      `arg:"--reject-path" help:"Path glob or regex (prefix with regex:) to exclude"`
	VisitedCachePath       string        `arg:"--visited-cache" help:"Optional file to persist visited URLs for resuming crawls"`
	HTTPTimeout            time.Duration `arg:"--http-timeout" help:"HTTP client timeout" default:"30s"`
	Aria2Args              []string      `arg:"positional" help:"Options forwarded to aria2c after the URL (use -- before them if they look like flags)"`
}

func main() {
	arg.MustParse(&args)

	if args.Url == "" {
		logrus.Fatal("please provide an URL")
	}

	parsedUrl, err := url.Parse(args.Url)
	if err != nil {
		logrus.Fatalf("invalid URL provided")
	}

	client := raria2.New(parsedUrl)
	client.OutputPath = args.Output
	client.Aria2AfterURLArgs = args.Aria2Args
	client.DryRun = args.DryRun
	client.HTTPTimeout = args.HTTPTimeout
	client.MaxDepth = args.MaxDepth
	client.AcceptExtensions = parseExtensionArgs(args.AcceptExtensions)
	client.RejectExtensions = parseExtensionArgs(args.RejectExtensions)
	client.VisitedCachePath = args.VisitedCachePath
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

	err = client.Run()
	if err != nil {
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
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
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
