package raria2

import (
	"net/url"
	"path"
	"strings"
)

// canonicalURL returns a normalized version of a URL for consistent comparison
// This function should be used everywhere URL comparison is needed
func canonicalURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	// Preserve whether the original URL looked like a directory (trailing slash in the *path*).
	// Important for URLs like: https://host/dir/?C=M;O=A
	// where raw does not end with '/', but the path does.
	hadTrailingSlash := parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/")

	// Normalize scheme to lowercase
	parsed.Scheme = strings.ToLower(parsed.Scheme)

	// Never persist credentials in canonical form: they must not end up in the
	// visited-URL cache file and do not change a resource's identity for dedup.
	parsed.User = nil

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
		case "ftp":
			if parsed.Port() == "21" {
				parsed.Host = parsed.Hostname()
			}
		case "ftps":
			if parsed.Port() == "990" {
				parsed.Host = parsed.Hostname()
			}
		}
	}

	// Always strip fragments
	parsed.Fragment = ""

	// Normalize path by cleaning dot-segments. Work on the normalized escaped
	// form so that percent-encoded slashes (e.g. %2F) stay distinct from
	// literal ones instead of collapsing into the same key.
	escaped := parsed.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	cleanedEscaped := path.Clean(normalizeEscapedPath(escaped))
	if cleanedEscaped == "." {
		cleanedEscaped = "/"
	}
	if unescaped, uerr := url.PathUnescape(cleanedEscaped); uerr == nil {
		parsed.Path = unescaped
		if cleanedEscaped == unescaped {
			parsed.RawPath = ""
		} else {
			parsed.RawPath = cleanedEscaped
		}
	} else {
		parsed.Path = path.Clean(parsed.Path)
		if parsed.Path == "." {
			parsed.Path = "/"
		}
		parsed.RawPath = ""
	}

	// Normalize path - but preserve root path
	if parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/") {
		// Remove trailing slashes for consistency (except root)
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
		if parsed.RawPath != "" {
			parsed.RawPath = strings.TrimSuffix(parsed.RawPath, "/")
		}
	}

	// For directory-style paths (ending with / in the original path) or root paths with query params,
	// clear query params (handles server directory listing sorting parameters).
	if hadTrailingSlash || (parsed.Path == "/" && parsed.RawQuery != "") {
		parsed.RawQuery = ""
	}

	return parsed.String()
}

// normalizeEscapedPath canonicalizes percent-escapes in a URL path: hex digits
// are uppercased and unreserved characters (RFC 3986) are decoded, so e.g.
// "%41" becomes "A" and "%2f" becomes "%2F". Escapes of reserved characters
// such as %2F are preserved so they remain distinguishable from literal ones.
func normalizeEscapedPath(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '%' && i+2 < len(p) {
			hi, ok1 := unhexDigit(p[i+1])
			lo, ok2 := unhexDigit(p[i+2])
			if ok1 && ok2 {
				v := hi<<4 | lo
				if isUnreservedByte(v) {
					b.WriteByte(v)
				} else {
					const hexDigits = "0123456789ABCDEF"
					b.WriteByte('%')
					b.WriteByte(hexDigits[hi])
					b.WriteByte(hexDigits[lo])
				}
				i += 2
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func unhexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func isUnreservedByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~':
		return true
	default:
		return false
	}
}

// logURL returns a URL string safe for logging (credentials redacted).
func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}

// SameUrl checks if two URLs are considered the same using canonicalization
func SameUrl(a *url.URL, b *url.URL) bool {
	return canonicalURL(a.String()) == canonicalURL(b.String())
}

// IsSubPath checks if subject URL is a sub-path of the base URL
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
		// Allow http -> https upgrades on the same host.
		if ofParsed.Scheme != "http" || subjectParsed.Scheme != "https" {
			return false
		}
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
