package proxy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfilerCreatesFiles(t *testing.T) {
	tmpDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &Profiler{
		ctx:        ctx,
		cancel:     cancel,
		dir:        tmpDir,
		currentDay: dailyTag(time.Now()),
	}

	p.writeSnapshot()

	data, err := os.ReadFile(filepath.Join(tmpDir, "metrics_snapshot.jsonl"))
	if err != nil {
		t.Fatalf("snapshot file not created: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("snapshot file is empty")
	}

	// JSONL: parse only the first line.
	firstLine := strings.SplitN(string(data), "\n", 2)[0]
	var snap ProfileSnapshot
	if err := json.Unmarshal([]byte(firstLine), &snap); err != nil {
		t.Fatalf("snapshot JSON invalid: %v", err)
	}
	if snap.Goroutines == 0 {
		t.Fatal("goroutines should be > 0")
	}
	if snap.Timestamp == "" {
		t.Fatal("timestamp should not be empty")
	}

	sumData, err := os.ReadFile(filepath.Join(tmpDir, "summary.jsonl"))
	if err != nil {
		t.Fatalf("summary file not created: %v", err)
	}
	if len(sumData) == 0 {
		t.Fatal("summary file is empty")
	}

	firstSumLine := strings.SplitN(string(sumData), "\n", 2)[0]
	var sumMap map[string]interface{}
	if err := json.Unmarshal([]byte(firstSumLine), &sumMap); err != nil {
		t.Fatalf("summary JSON invalid: %v", err)
	}
	if _, ok := sumMap["goroutines"]; !ok {
		t.Fatal("summary should contain goroutines field")
	}
}

func TestProfilerGoroutineDump(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &Profiler{ctx: ctx, cancel: cancel, dir: tmpDir, currentDay: dailyTag(time.Now())}
	p.writeGoroutineDump()

	data, err := os.ReadFile(filepath.Join(tmpDir, "goroutine_dump.txt"))
	if err != nil {
		t.Fatalf("goroutine dump not created: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("goroutine dump is empty")
	}
	if !strings.Contains(string(data), "Goroutine dump at") {
		t.Fatal("goroutine dump missing header")
	}
	if !strings.Contains(string(data), "goroutine ") {
		t.Fatal("goroutine dump missing stack traces")
	}
}

func TestProfilerStopCancels(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel

	p := &Profiler{ctx: ctx, cancel: cancel, dir: tmpDir, currentDay: dailyTag(time.Now())}
	p.Start()
	p.Stop()

	select {
	case <-p.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("profiler context should be cancelled after Stop")
	}

	// Give background goroutines time to finish so TempDir cleanup succeeds.
	time.Sleep(200 * time.Millisecond)
}

func TestProfilerRotateFiles(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &Profiler{ctx: ctx, cancel: cancel, dir: tmpDir, currentDay: "2025-01-15"}

	p.writeSnapshot()
	p.writeGoroutineDump()

	for _, name := range []string{"metrics_snapshot.jsonl", "summary.jsonl", "goroutine_dump.txt"} {
		if _, err := os.Stat(filepath.Join(tmpDir, name)); err != nil {
			t.Fatalf("expected %s to exist before rotation: %v", name, err)
		}
	}

	err := p.rotateFiles("2025-01-15")
	if err != nil {
		t.Fatalf("rotateFiles failed: %v", err)
	}

	for _, name := range []string{"metrics_snapshot.jsonl", "summary.jsonl", "goroutine_dump.txt"} {
		if _, err := os.Stat(filepath.Join(tmpDir, name)); err == nil {
			t.Fatalf("expected %s to be removed after rotation", name)
		}
	}

	zipPath := filepath.Join(tmpDir, "profile_2025-01-15.zip")
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("expected zip archive: %v", err)
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("failed to open zip: %v", err)
	}
	defer r.Close()

	fileNames := make(map[string]bool)
	for _, f := range r.File {
		fileNames[f.Name] = true
		if f.Method != zip.Deflate {
			t.Errorf("expected Deflate compression for %s, got %d", f.Name, f.Method)
		}
	}

	for _, expected := range []string{
		"2025-01-15/metrics_snapshot.jsonl",
		"2025-01-15/summary.jsonl",
		"2025-01-15/goroutine_dump.txt",
	} {
		if !fileNames[expected] {
			t.Errorf("missing file in zip: %s", expected)
		}
	}
}

func TestProfilerCleanOldArchives(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &Profiler{ctx: ctx, cancel: cancel, dir: tmpDir, currentDay: dailyTag(time.Now())}

	for i := 1; i <= 7; i++ {
		name := filepath.Join(tmpDir, fmt.Sprintf("profile_2025-01-%02d.zip", i))
		os.WriteFile(name, []byte("fake zip"), 0o644)
	}

	p.cleanOldArchives()

	entries, _ := os.ReadDir(tmpDir)
	var remaining []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			remaining = append(remaining, e.Name())
		}
	}

	if len(remaining) != maxRotatedFiles {
		t.Fatalf("expected %d archives, got %d: %v", maxRotatedFiles, len(remaining), remaining)
	}

	for _, name := range remaining {
		if name == "profile_2025-01-01.zip" || name == "profile_2025-01-02.zip" {
			t.Errorf("old archive %s should have been removed", name)
		}
	}
}

func TestProfilerCheckRotate(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	p := &Profiler{ctx: ctx, cancel: cancel, dir: tmpDir, currentDay: yesterday}

	p.writeSnapshot()

	p.checkRotate()

	if p.currentDay != dailyTag(time.Now()) {
		t.Fatal("currentDay not updated after rotation")
	}

	zipPath := filepath.Join(tmpDir, fmt.Sprintf("profile_%s.zip", yesterday))
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("expected zip archive for yesterday: %v", err)
	}
}
