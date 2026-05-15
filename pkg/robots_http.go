package raria2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/temoto/robotstxt"
)

type robotsCall struct {
	wg  sync.WaitGroup
	res *robotstxt.RobotsData
	err error
}

func (r *RAria2) urlAllowedByRobots(ctx context.Context, u *url.URL) bool {
	if !r.RespectRobots {
		return true
	}

	robots, err := r.getRobotsData(ctx, u.Scheme, u.Host)
	if err != nil {
		logrus.Debugf("failed to fetch robots.txt for %s: %v", u.Host, err)
		return true // fail open
	}

	return robots.TestAgent(u.Path, r.UserAgent)
}

func (r *RAria2) getRobotsData(ctx context.Context, scheme, host string) (*robotstxt.RobotsData, error) {
	r.robotsCacheMu.RLock()
	if r.robotsCache != nil {
		if cached, ok := r.robotsCache[host]; ok {
			r.robotsCacheMu.RUnlock()
			return cached, nil
		}
	}
	r.robotsCacheMu.RUnlock()

	// Singleflight: only one fetch per host.
	r.robotsInflightMu.Lock()
	if r.robotsInflight == nil {
		r.robotsInflight = make(map[string]*robotsCall)
	}
	call, ok := r.robotsInflight[host]
	if !ok {
		call = &robotsCall{}
		call.wg.Add(1)
		r.robotsInflight[host] = call
		r.robotsInflightMu.Unlock()

		call.res, call.err = r.fetchRobotsData(ctx, scheme, host)

		call.wg.Done()
		r.robotsInflightMu.Lock()
		delete(r.robotsInflight, host)
		r.robotsInflightMu.Unlock()
	} else {
		r.robotsInflightMu.Unlock()
		call.wg.Wait()
	}
	return call.res, call.err
}

func (r *RAria2) fetchRobotsData(ctx context.Context, scheme, host string) (*robotstxt.RobotsData, error) {
	if scheme != "https" {
		scheme = "http"
	}
	robotsURL := fmt.Sprintf("%s://%s/robots.txt", scheme, host)
	req, err := http.NewRequestWithContext(ctx, "GET", robotsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.UserAgent)

	resp, err := r.doRobotsRequest(req)
	if err != nil {
		otherScheme := "https"
		if scheme == "https" {
			otherScheme = "http"
		}
		robotsURL = fmt.Sprintf("%s://%s/robots.txt", otherScheme, host)
		req, err = http.NewRequestWithContext(ctx, "GET", robotsURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", r.UserAgent)
		resp, err = r.doRobotsRequest(req)
		if err != nil {
			robotsData := robotstxt.RobotsData{}
			r.cacheRobotsData(host, &robotsData)
			return &robotsData, nil
		}
	}
	defer closeQuietly(resp.Body)

	if resp.StatusCode != http.StatusOK {
		robotsData := robotstxt.RobotsData{}
		r.cacheRobotsData(host, &robotsData)
		return &robotsData, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	robotsData, err := robotstxt.FromBytes(body)
	if err != nil {
		return nil, err
	}

	r.cacheRobotsData(host, robotsData)
	return robotsData, nil
}

func (r *RAria2) doRobotsRequest(req *http.Request) (*http.Response, error) {
	if r.DisableRetries {
		return r.client().Do(req)
	}
	return r.client().DoWithRetry(req)
}

func (r *RAria2) cacheRobotsData(host string, data *robotstxt.RobotsData) {
	r.robotsCacheMu.Lock()
	if r.robotsCache == nil {
		r.robotsCache = make(map[string]*robotstxt.RobotsData)
	}
	r.robotsCache[host] = data
	r.robotsCacheMu.Unlock()
}
