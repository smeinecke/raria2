package raria2

import (
	"context"
	"fmt"
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

// ftpListError maps a failed LIST attempt: paths that do not look like
// directories are reported as errNotHTML so the caller retries them as file
// downloads, while real directory-listing failures surface as errors.
func ftpListError(u *url.URL, err error) error {
	if !strings.HasSuffix(u.Path, "/") {
		return errNotHTML
	}
	return fmt.Errorf("FTP LIST failed for %s: %w", u.Path, err)
}

func (r *RAria2) ftpListEntries(ctx context.Context, u *url.URL) ([]ftpListingEntry, error) {
	var out []ftpListingEntry
	if r.ftpList != nil {
		var err error
		out, err = r.ftpList(ctx, u)
		if err != nil {
			return nil, err
		}
	} else {
		entries, err := r.ftpListRaw(ctx, u)
		if err != nil {
			return nil, err
		}

		out = make([]ftpListingEntry, 0, len(entries))
		for _, e := range entries {
			if e == nil {
				continue
			}
			if e.Name == "" || e.Name == "." || e.Name == ".." {
				continue
			}
			// Symbolic links are skipped on purpose: their target type is
			// unknown and link cycles would produce unbounded crawl paths.
			if e.Type == ftp.EntryTypeLink {
				continue
			}
			out = append(out, ftpListingEntry{name: e.Name, isDir: e.Type == ftp.EntryTypeFolder})
		}
	}

	if !strings.HasSuffix(u.Path, "/") && len(out) == 1 && !out[0].isDir && matchesFTPPathName(out[0].name, u) {
		// Ambiguous: a directory containing a single file named like the
		// directory produces the exact same listing. Consult the parent
		// listing to find out what u.Path really is.
		if !r.ftpIsDirectory(ctx, u) {
			return nil, errNotHTML
		}
	}

	return out, nil
}

// matchesFTPPathName reports whether a LIST entry name refers to u itself;
// servers variously return the basename or the full server-side path.
func matchesFTPPathName(name string, u *url.URL) bool {
	return name == path.Base(u.Path) || name == strings.TrimPrefix(u.Path, "/") || name == u.Path
}

// ftpListRaw performs the LIST request over the connection pool, with a fresh
// connection retry on failure.
func (r *RAria2) ftpListRaw(ctx context.Context, u *url.URL) ([]*ftp.Entry, error) {
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
			return nil, ftpListError(u, err)
		}

		logger2 := logrus.WithFields(entry2.logFields()).WithField("list_path", listPath)
		logger2.Debug("FTP LIST: retry requesting directory entries")
		entries, err = ftpList(ctx, entry2.conn, listPath)
		if err != nil {
			logger2.WithError(err).Debug("FTP LIST: retry failed; discarding connection")
			pool2.discard(entry2)
			return nil, ftpListError(u, err)
		}
		logger2.WithField("entry_count", len(entries)).Debug("FTP LIST: retry succeeded")
		pool2.put(entry2)
	} else {
		pool.put(entry)
	}

	logger.WithField("entry_count", len(entries)).Debug("FTP LIST: request succeeded")
	return entries, nil
}

// ftpIsDirectory disambiguates a path whose LIST returned a single same-named
// file entry by looking the path up in its parent directory listing. When the
// type cannot be determined it returns false so callers keep treating the
// path as a file.
func (r *RAria2) ftpIsDirectory(ctx context.Context, u *url.URL) bool {
	parent := *u
	parentPath := path.Dir(u.Path)
	if parentPath == "." || parentPath == "" {
		parentPath = "/"
	}
	if !strings.HasSuffix(parentPath, "/") {
		parentPath += "/"
	}
	parent.Path = parentPath
	parent.RawPath = ""

	parentEntries, err := r.ftpListEntries(ctx, &parent)
	if err != nil {
		return false
	}
	for _, e := range parentEntries {
		if matchesFTPPathName(e.name, u) {
			return e.isDir
		}
	}
	return false
}
