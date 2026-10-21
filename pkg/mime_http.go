package raria2

import (
	"context"
	"io"
	"net/http"
)

func (r *RAria2) sniffHTTPContentTypeWithContext(ctx context.Context, workerId int, cUrl string) (string, bool) {
	contentType := ""
	req, err := http.NewRequestWithContext(ctx, "HEAD", cUrl, nil)
	if err == nil {
		req.Header.Set("User-Agent", r.UserAgent)
		var res *http.Response
		if r.DisableRetries {
			res, err = r.client().Do(req)
		} else {
			res, err = r.client().DoWithRetry(req)
		}
		if err == nil {
			closeQuietly(res.Body)
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				contentType = res.Header.Get("Content-Type")
			}
		}
	}

	// If HEAD failed or did not provide a usable content-type, sniff via a small GET.
	if contentType == "" {
		sniffRes, err := r.sniffGet(ctx, cUrl, true)
		if err != nil {
			return "", false
		}
		if sniffRes != nil {
			// Some servers reject Range requests; retry without it like
			// IsHtmlPageWithContext does.
			if sniffRes.StatusCode == 416 || sniffRes.StatusCode == 400 {
				closeQuietly(sniffRes.Body)
				sniffRes, err = r.sniffGet(ctx, cUrl, false)
				if err != nil {
					return "", false
				}
			}
			if sniffRes != nil {
				defer closeQuietly(sniffRes.Body)
				if sniffRes.StatusCode >= 200 && sniffRes.StatusCode < 300 {
					limitReader := io.LimitReader(sniffRes.Body, 1024)
					bodyBytes, _ := io.ReadAll(limitReader)
					contentType = http.DetectContentType(bodyBytes)
				}
			}
		}
	}

	return contentType, true
}

// sniffGet issues a GET request for content-type sniffing. When useRange is
// true, only the first KiB is requested.
func (r *RAria2) sniffGet(ctx context.Context, cUrl string, useRange bool) (*http.Response, error) {
	sniffReq, err := http.NewRequestWithContext(ctx, "GET", cUrl, nil)
	if err != nil {
		return nil, err
	}
	sniffReq.Header.Set("User-Agent", r.UserAgent)
	if useRange {
		sniffReq.Header.Set("Range", "bytes=0-1023")
	}
	if r.DisableRetries {
		return r.client().Do(sniffReq)
	}
	return r.client().DoWithRetry(sniffReq)
}
