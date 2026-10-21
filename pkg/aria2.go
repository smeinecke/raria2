package raria2

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// aria2URLEntry represents a single download entry for aria2
type aria2URLEntry struct {
	URL string
	Dir string
	Out string
}

// downloadSink interface for different output destinations
type downloadSink interface {
	Write(entry aria2URLEntry) error
	Close() error
}

// Aria2Manager handles aria2 integration and batch processing
type Aria2Manager struct {
	MaxConnectionPerServer int
	MaxConcurrentDownload  int
	Aria2EntriesPerSession int
	DryRun                 bool
	Aria2AfterURLArgs      []string
	WriteBatch             string
	SkipCertificateCheck   bool
	// Aria2StreamingAuto probes aria2 once per run for incremental stdin
	// support (aria2/aria2#1161). A patched binary consumes -i - entries
	// as they arrive, so session restarts are skipped entirely.
	Aria2StreamingAuto  bool
	sinkFactory         func(context.Context, *RAria2) (downloadSink, error)
	sessionLimitOnce    sync.Once
	resolvedSessionSize int
}

// NewAria2Manager creates a new aria2 manager
func NewAria2Manager() *Aria2Manager {
	return &Aria2Manager{
		MaxConnectionPerServer: 5,
		MaxConcurrentDownload:  5,
		Aria2EntriesPerSession: 100,
	}
}

// sessionEntryLimit returns the session limit based on configuration
func (am *Aria2Manager) sessionEntryLimit() int {
	am.sessionLimitOnce.Do(func() {
		am.resolvedSessionSize = am.resolveSessionLimit()
	})
	return am.resolvedSessionSize
}

func (am *Aria2Manager) resolveSessionLimit() int {
	if am.Aria2EntriesPerSession <= 0 {
		return 0
	}
	if am.WriteBatch != "" {
		logrus.Warn("--aria2-session-size is ignored when --write-batch is set")
		return 0
	}
	if am.Aria2StreamingAuto && !am.DryRun && aria2StreamingProbe() {
		logrus.Info("aria2 supports incremental stdin input; sending URLs without session restarts")
		return 0
	}
	return am.Aria2EntriesPerSession
}

// ExecuteBatchDownload processes download entries using the appropriate sink
func (am *Aria2Manager) ExecuteBatchDownload(ctx context.Context, raria2 *RAria2, entries <-chan aria2URLEntry) error {
	var (
		sink              downloadSink
		sinkErr           error
		entryCount        int
		sessionEntryCount int
	)

	sessionLimit := am.sessionEntryLimit()
	flushSession := func() error {
		if sink == nil {
			sessionEntryCount = 0
			return nil
		}
		if err := sink.Close(); err != nil {
			return err
		}
		sink = nil
		sessionEntryCount = 0
		return nil
	}

	for entry := range entries {
		if sink == nil {
			sink, sinkErr = am.newDownloadSink(ctx, raria2)
			if sinkErr != nil {
				return sinkErr
			}
		}
		if err := sink.Write(entry); err != nil {
			// A malformed entry (e.g. control characters from a hostile
			// listing) must not abort the whole download run.
			if errors.Is(err, errInvalidBatchEntry) {
				logrus.Warnf("skipping invalid batch entry %s: %v", logURL(entry.URL), err)
				continue
			}
			_ = flushSession()
			return err
		}
		entryCount++
		sessionEntryCount++
		if sessionLimit > 0 && sessionEntryCount >= sessionLimit {
			if err := flushSession(); err != nil {
				return err
			}
		}
	}

	if entryCount == 0 {
		logrus.Info("no downloadable resources found")
		return nil
	}

	if err := flushSession(); err != nil {
		return err
	}

	if am.WriteBatch != "" {
		logrus.Infof("wrote aria2 batch file with %d download entries to %s", entryCount, am.WriteBatch)
	}

	return nil
}

// discardSink accepts download entries without writing them anywhere. It is
// used for dry runs so that no aria2c process (or binary requirement) is
// needed to preview what would be downloaded.
type discardSink struct{}

func (discardSink) Write(aria2URLEntry) error { return nil }
func (discardSink) Close() error              { return nil }

// newDownloadSink creates the appropriate download sink
func (am *Aria2Manager) newDownloadSink(ctx context.Context, raria2 *RAria2) (downloadSink, error) {
	if am.WriteBatch != "" {
		return newBatchFileSink(am.WriteBatch)
	}
	if am.sinkFactory != nil {
		return am.sinkFactory(ctx, raria2)
	}
	if am.DryRun {
		return discardSink{}, nil
	}
	return newAria2Sink(ctx, am)
}

// aria2Sink implements downloadSink for direct aria2 execution
type aria2Sink struct {
	writer    *bufio.Writer
	pipe      *io.PipeWriter
	waitCh    chan error
	closeOnce sync.Once
	closeErr  error
}

// newAria2Sink creates a new aria2 sink
func newAria2Sink(ctx context.Context, am *Aria2Manager) (downloadSink, error) {
	binFile := "aria2c"
	args := []string{"-x", strconv.Itoa(am.MaxConnectionPerServer)}
	if am.MaxConcurrentDownload > 0 {
		args = append(args, "-j", strconv.Itoa(am.MaxConcurrentDownload))
	}
	args = append(args, "--input-file", "-", "--deferred-input=true")
	args = append(args, "--follow-torrent=false")

	if am.DryRun {
		args = append(args, "--dry-run=true")
	}

	if am.SkipCertificateCheck {
		args = append(args, "--check-certificate=false")
	}

	if len(am.Aria2AfterURLArgs) > 0 {
		args = append(args, am.Aria2AfterURLArgs...)
	}

	if am.DryRun {
		fmt.Printf("aria2 batch cmd: %s %s\n", binFile, strings.Join(args, " "))
	}

	reader, writer := io.Pipe()
	cmd := execCommand(binFile, args...)
	cmd.Stdin = reader
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, fmt.Errorf("failed to start aria2c: %w", err)
	}

	waitCh := make(chan error, 1)
	processDone := make(chan struct{})
	go func() {
		waitCh <- cmd.Wait()
		close(processDone)
	}()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			case <-processDone:
			}
		}()
	}

	return &aria2Sink{
		writer: bufio.NewWriter(writer),
		pipe:   writer,
		waitCh: waitCh,
	}, nil
}

// Write writes an entry to the aria2 sink
func (s *aria2Sink) Write(entry aria2URLEntry) error {
	if err := writeBatchEntry(s.writer, entry); err != nil {
		return err
	}
	return s.writer.Flush()
}

// Close closes the aria2 sink
func (s *aria2Sink) Close() error {
	s.closeOnce.Do(func() {
		if err := s.writer.Flush(); err != nil {
			s.closeErr = err
		}
		// Always close the pipe and wait for the process so a failed flush
		// does not leave aria2c blocked on stdin forever.
		if err := s.pipe.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
		if waitErr := <-s.waitCh; waitErr != nil && s.closeErr == nil {
			s.closeErr = fmt.Errorf("aria2 batch command failed: %w", waitErr)
		}
	})
	return s.closeErr
}

// batchFileSink implements downloadSink for file output
type batchFileSink struct {
	writer    *bufio.Writer
	file      *os.File
	closeOnce sync.Once
	closeErr  error
}

// newBatchFileSink creates a new batch file sink
func newBatchFileSink(path string) (downloadSink, error) {
	file, err := os.Create(path) // #nosec G304 -- path is the user-provided batch output flag
	if err != nil {
		return nil, fmt.Errorf("failed to create batch file %s: %w", path, err)
	}
	return &batchFileSink{writer: bufio.NewWriter(file), file: file}, nil
}

// Write writes an entry to the batch file sink
func (s *batchFileSink) Write(entry aria2URLEntry) error {
	return writeBatchEntry(s.writer, entry)
}

// Close closes the batch file sink
func (s *batchFileSink) Close() error {
	s.closeOnce.Do(func() {
		if err := s.writer.Flush(); err != nil {
			s.closeErr = err
			return
		}
		if err := s.file.Close(); err != nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// errInvalidBatchEntry marks download entries that cannot be serialized into
// an aria2 input file. It is distinct from sink I/O errors so the batch loop
// can skip the offending entry instead of aborting the run.
var errInvalidBatchEntry = errors.New("batch entry contains control characters")

// aria2StreamingProbe is a seam so tests can stub the real binary probe.
var aria2StreamingProbe = probeAria2StdinStreaming

// probeDispatchMarkers lists console output fragments that prove aria2
// dispatched a stdin-fed URL while the input pipe is still open. An
// unpatched aria2 blocks inside fgets and prints nothing in that window.
var probeDispatchMarkers = []string{
	"Download aborted",
	"Download complete",
	"Downloading ",
	"errorCode",
	"[#",
}

// probeAria2StdinStreaming reports whether the aria2c binary on PATH consumes
// -i - entries incrementally (i.e. includes the fix for aria2/aria2#1161).
// It feeds one instantly-failing URL, keeps stdin open, and watches for
// dispatch activity. The probe process is always killed afterwards.
func probeAria2StdinStreaming() bool {
	const (
		decisionWindow = 1500 * time.Millisecond
		overallTimeout = 5 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), overallTimeout)
	defer cancel()

	cmd := execCommand("aria2c",
		"--input-file", "-", "--deferred-input=true",
		"--console-log-level=notice", "--summary-interval=0",
		"--max-tries=1", "--retry-wait=0",
		"--connect-timeout=2", "--timeout=2")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	outR, outW := io.Pipe()
	cmd.Stdout = outW
	cmd.Stderr = outW

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		defer func() { _ = outR.Close() }()
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	if err := cmd.Start(); err != nil {
		_ = outW.Close()
		return false
	}

	// Keep stdin open: patched aria2 dispatches the entry immediately,
	// unpatched aria2 blocks in fgets until EOF.
	if _, err := io.WriteString(stdin, "http://127.0.0.1:1/raria2-streaming-probe\n"); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return false
	}

	streaming := false
	timer := time.NewTimer(decisionWindow)
	defer timer.Stop()

loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// Process closed its output - either exited or crashed;
				// a patched binary may exit after the failed download if
				// stdin closed early, treat any marker seen as positive.
				break loop
			}
			for _, marker := range probeDispatchMarkers {
				if strings.Contains(line, marker) {
					streaming = true
					break loop
				}
			}
		case <-timer.C:
			break loop
		case <-ctx.Done():
			break loop
		}
	}

	_ = stdin.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	return streaming
}

func containsControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// writeBatchEntry writes a single entry to the batch writer
func writeBatchEntry(writer *bufio.Writer, entry aria2URLEntry) error {
	if containsControlChar(entry.URL) || containsControlChar(entry.Dir) || containsControlChar(entry.Out) {
		return errInvalidBatchEntry
	}
	if _, err := fmt.Fprintln(writer, entry.URL); err != nil {
		return err
	}
	if entry.Dir != "" {
		if _, err := fmt.Fprintf(writer, "  dir=%s\n", entry.Dir); err != nil {
			return err
		}
	}
	if entry.Out != "" {
		if _, err := fmt.Fprintf(writer, "  out=%s\n", entry.Out); err != nil {
			return err
		}
	}
	if _, err := writer.WriteString("\n"); err != nil {
		return err
	}
	return nil
}
