package proxy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

const (
	profileDir       = "profile_debug"
	snapshotInterval = 10 * time.Second
	maxRotatedFiles  = 5
	rotateCheckInterval = 1 * time.Minute
)

// ProfileSnapshot is the periodic record written to profile_debug/.
type ProfileSnapshot struct {
	Timestamp     string                      `json:"timestamp"`
	UnixTimestamp int64                       `json:"unix_timestamp"`
	Goroutines    int                         `json:"goroutines"`
	Metrics       connmonitor.MetricsSnapshot `json:"metrics"`
	Connections   connmonitor.Summary         `json:"connections"`
	Channels      []connmonitor.ChannelSample `json:"channels"`
}

// Profiler writes periodic performance snapshots when debug mode is enabled.
// Files are rotated daily (one set per day), compressed to .zip, and old
// archives beyond maxRotatedFiles are deleted.
type Profiler struct {
	ctx       context.Context
	cancel    context.CancelFunc
	dir       string
	currentDay string // "2006-01-02" of the current open files
}

func dailyTag(t time.Time) string {
	return t.Format("2006-01-02")
}

func (p *Profiler) snapshotPath() string {
	return filepath.Join(p.dir, "metrics_snapshot.jsonl")
}

func (p *Profiler) summaryPath() string {
	return filepath.Join(p.dir, "summary.jsonl")
}

func (p *Profiler) goroutinePath() string {
	return filepath.Join(p.dir, "goroutine_dump.txt")
}

// NewProfiler creates a profiler that writes to profile_debug/.
// The directory is resolved to an absolute path relative to the executable
// so that it works correctly regardless of the process working directory.
func NewProfiler(ctx context.Context) (*Profiler, error) {
	dir := profileDir
	// Resolve to absolute path next to the executable so systemd / init
	// scripts that set WorkingDirectory=/ don't scatter files.
	if !filepath.IsAbs(dir) {
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Join(filepath.Dir(exe), dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("profiler: mkdir %s: %w", dir, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	p := &Profiler{
		ctx:        ctx,
		cancel:     cancel,
		dir:        dir,
		currentDay: dailyTag(time.Now()),
	}
	return p, nil
}

// Start begins the periodic snapshot loop in background.
func (p *Profiler) Start() {
	log.Info("profiler: debug mode enabled, writing to ", p.dir)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("profiler: initial snapshot panic: ", r)
			}
		}()
		log.Info("profiler: writing initial snapshot...")
		p.writeSnapshot()
		log.Info("profiler: initial snapshot written")
		p.writeGoroutineDump()
		log.Info("profiler: initial goroutine dump written")
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("profiler: snapshot loop panic: ", r)
			}
		}()
		p.loop()
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("profiler: goroutine dump loop panic: ", r)
			}
		}()
		p.goroutineDumpLoop()
	}()
}

// Stop halts the profiler and rotates the current day's files.
func (p *Profiler) Stop() {
	p.cancel()
}

func (p *Profiler) loop() {
	ticker := time.NewTicker(snapshotInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.checkRotate()
			p.writeSnapshot()
		}
	}
}

func (p *Profiler) goroutineDumpLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.writeGoroutineDump()
		}
	}
}

// checkRotate checks if the day has changed. If so, it compresses the
// current day's files into a .zip archive and cleans up old archives.
func (p *Profiler) checkRotate() {
	today := dailyTag(time.Now())
	if today == p.currentDay {
		return
	}

	prevDay := p.currentDay
	p.currentDay = today

	log.Info("profiler: rotating files for ", prevDay)
	if err := p.rotateFiles(prevDay); err != nil {
		log.Warn("profiler: rotation error: ", err)
	}
	p.cleanOldArchives()
}

// rotateFiles compresses the current active files into a dated zip archive
// and removes the originals.
func (p *Profiler) rotateFiles(dayTag string) error {
	activeFiles := []string{
		p.snapshotPath(),
		p.summaryPath(),
		p.goroutinePath(),
	}

	// Collect files that actually exist.
	var toArchive []string
	for _, f := range activeFiles {
		if _, err := os.Stat(f); err == nil {
			toArchive = append(toArchive, f)
		}
	}
	if len(toArchive) == 0 {
		return nil
	}

	zipPath := filepath.Join(p.dir, fmt.Sprintf("profile_%s.zip", dayTag))
	if err := createZipArchive(zipPath, toArchive, dayTag); err != nil {
		return fmt.Errorf("create zip %s: %w", zipPath, err)
	}

	// Remove originals after successful archiving.
	for _, f := range toArchive {
		os.Remove(f)
	}

	log.Info("profiler: archived to ", zipPath)
	return nil
}

// createZipArchive compresses the given files into a zip archive.
// Files are stored inside the zip with a dayTag prefix directory.
func createZipArchive(zipPath string, files []string, dayTag string) error {
	zf, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer zf.Close()

	w := zip.NewWriter(zf)
	defer w.Close()

	for _, fpath := range files {
		src, err := os.Open(fpath)
		if err != nil {
			return err
		}

		info, err := src.Stat()
		if err != nil {
			src.Close()
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			src.Close()
			return err
		}
		header.Name = dayTag + "/" + filepath.Base(fpath)
		header.Method = zip.Deflate

		dst, err := w.CreateHeader(header)
		if err != nil {
			src.Close()
			return err
		}

		_, err = io.Copy(dst, src)
		src.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// cleanOldArchives removes zip archives beyond the maxRotatedFiles limit,
// keeping the most recent ones.
func (p *Profiler) cleanOldArchives() {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return
	}

	var archives []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "profile_") && strings.HasSuffix(name, ".zip") {
			archives = append(archives, name)
		}
	}

	if len(archives) <= maxRotatedFiles {
		return
	}

	// Sort lexicographically — date-based names sort chronologically.
	sort.Strings(archives)

	// Remove the oldest archives.
	toRemove := archives[:len(archives)-maxRotatedFiles]
	for _, name := range toRemove {
		path := filepath.Join(p.dir, name)
		if err := os.Remove(path); err != nil {
			log.Warn("profiler: failed to remove old archive: ", err)
		} else {
			log.Info("profiler: removed old archive ", name)
		}
	}
}

func (p *Profiler) writeSnapshot() {
	now := time.Now()
	metrics := connmonitor.GlobalMetrics().Snapshot()
	monitor := connmonitor.Global()
	summary := monitor.GetSummary()

	snap := ProfileSnapshot{
		Timestamp:     now.Format(time.RFC3339),
		UnixTimestamp: now.Unix(),
		Goroutines:    runtime.NumGoroutine(),
		Metrics:       metrics,
		Connections:   summary,
		Channels:      metrics.Channels,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		log.Warn("profiler: marshal error: ", err)
		return
	}
	data = append(data, '\n')

	snapshotPath := p.snapshotPath()
	if err := appendFile(snapshotPath, data); err != nil {
		log.Warn("profiler: write snapshot error: ", snapshotPath, " ", err)
		return
	}
	log.Info("profiler: snapshot written to ", snapshotPath)

	summaryLine := fmt.Sprintf(
		"{\"time\":%q,\"goroutines\":%d,\"active_conns\":%d,\"total_conns\":%d,"+
			"\"up_speed\":%.0f,\"down_speed\":%.0f,"+
			"\"ttfb_p50_ms\":%.2f,\"ttfb_p95_ms\":%.2f,"+
			"\"heap_mb\":%.1f,\"gc_pause_ms\":%.3f,"+
			"\"conn_open_cps\":%d,\"conn_close_cps\":%d,"+
			"\"backpressure_events\":%d}\n",
		now.Format("15:04:05"),
		snap.Goroutines,
		summary.ActiveConnections,
		summary.TotalConnections,
		summary.TotalUploadSpeed,
		summary.TotalDownloadSpeed,
		metrics.TTFBP50Ms, metrics.TTFBP95Ms,
		metrics.HeapAllocMB, metrics.GCPauseLastMs,
		metrics.OpenCPS, metrics.CloseCPS,
		metrics.BackpressureEvents,
	)

	summaryPath := p.summaryPath()
	if err := appendFile(summaryPath, []byte(summaryLine)); err != nil {
		log.Warn("profiler: write summary error: ", summaryPath, " ", err)
	}
}

func (p *Profiler) writeGoroutineDump() {
	buf := make([]byte, 1<<20) // 1 MB
	n := runtime.Stack(buf, true)
	header := fmt.Sprintf("\n=== Goroutine dump at %s (count=%d) ===\n",
		time.Now().Format(time.RFC3339), runtime.NumGoroutine())

	data := append([]byte(header), buf[:n]...)
	data = append(data, '\n')

	if err := appendFile(p.goroutinePath(), data); err != nil {
		log.Warn("profiler: goroutine dump error: ", err)
	}
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
