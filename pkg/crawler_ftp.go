package raria2

import (
	"context"
	"net/url"
	"path"
	"strings"

	"github.com/jlaffaye/ftp"
	"github.com/sirupsen/logrus"
)

type ftpListingEntry struct {
	name  string
	isDir bool
}

func (r *RAria2) getLinksByFTPWithContext(ctx context.Context, parsedURL *url.URL) ([]string, error) {
	entries, err := r.ftpListEntries(ctx, parsedURL)
	if err != nil {
		return nil, err
	}

	dirPath := parsedURL.Path
	if dirPath == "" {
		dirPath = "/"
	}
	if !strings.HasSuffix(dirPath, "/") {
		dirPath += "/"
	}

	links := make([]string, 0, len(entries))
	for _, e := range entries {
		child := *parsedURL
		child.Fragment = ""
		child.RawQuery = ""
		child.RawPath = ""
		child.Path = path.Join(dirPath, e.name)
		if e.isDir && !strings.HasSuffix(child.Path, "/") {
			child.Path += "/"
		}
		links = append(links, child.String())
	}

	return links, nil
}

func ftpList(ctx context.Context, conn *ftp.ServerConn, listPath string) ([]*ftp.Entry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	entries, err := conn.List(listPath)
	if err != nil && !strings.HasSuffix(listPath, "/") {
		entries, err = conn.List(listPath + "/")
	}
	return entries, err
}

func (r *RAria2) ftpListEntries(ctx context.Context, u *url.URL) ([]ftpListingEntry, error) {
	if r.ftpList != nil {
		return r.ftpList(ctx, u)
	}

	listPath := u.Path
	if listPath == "" {
		listPath = "/"
	}

	pool, entry, err := r.ftpConnPoolGet(ctx, u)
	if err != nil {
		return nil, err
	}

	logger := logrus.WithFields(entry.logFields()).WithField("list_path", listPath)
	logger.Debug("FTP LIST: requesting directory entries")

	entries, err := ftpList(ctx, entry.conn, listPath)
	if err != nil {
		logger.WithError(err).Debug("FTP LIST: request failed; discarding connection")
		// Discard the broken connection and retry with a fresh one.
		pool.discard(entry)

		pool2, entry2, reErr := r.ftpConnPoolGet(ctx, u)
		if reErr != nil {
			logger.WithError(reErr).Debug("FTP LIST: retry failed acquiring new connection")
			return nil, errNotHTML
		}

		logger2 := logrus.WithFields(entry2.logFields()).WithField("list_path", listPath)
		logger2.Debug("FTP LIST: retry requesting directory entries")
		entries, err = ftpList(ctx, entry2.conn, listPath)
		if err != nil {
			logger2.WithError(err).Debug("FTP LIST: retry failed; discarding connection")
			pool2.discard(entry2)
			return nil, errNotHTML
		}
		logger2.WithField("entry_count", len(entries)).Debug("FTP LIST: retry succeeded")
		pool2.put(entry2)
	} else {
		pool.put(entry)
	}

	logger.WithField("entry_count", len(entries)).Debug("FTP LIST: request succeeded")

	if !strings.HasSuffix(u.Path, "/") {
		base := path.Base(u.Path)
		if len(entries) == 1 && entries[0] != nil && entries[0].Name == base && entries[0].Type == ftp.EntryTypeFile {
			return nil, errNotHTML
		}
	}

	out := make([]ftpListingEntry, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		if e.Name == "" || e.Name == "." || e.Name == ".." {
			continue
		}
		if e.Type == ftp.EntryTypeLink {
			continue
		}
		out = append(out, ftpListingEntry{name: e.Name, isDir: e.Type == ftp.EntryTypeFolder})
	}

	return out, nil
}
