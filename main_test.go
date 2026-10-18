package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWalkAllAndBuildTree(t *testing.T) {
	tmp := t.TempDir()

	// Create directory hierarchy:
	// tmp/
	//   a/
	//     file1 (64 KiB)
	//     file1_hardlink (hardlink to file1)
	//     sub/
	//       file2 (32 KiB)
	//   empty/
	dirA := filepath.Join(tmp, "a")
	dirSub := filepath.Join(dirA, "sub")
	dirEmpty := filepath.Join(tmp, "empty")

	if err := os.MkdirAll(dirSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirEmpty, 0o755); err != nil {
		t.Fatal(err)
	}

	file1 := filepath.Join(dirA, "file1")
	data1 := make([]byte, 64*1024)
	if err := os.WriteFile(file1, data1, 0o644); err != nil {
		t.Fatal(err)
	}

	// Hardlink to file1 — should be deduplicated and counted only once.
	file1Link := filepath.Join(dirA, "file1_hardlink")
	if err := os.Link(file1, file1Link); err != nil {
		t.Fatal(err)
	}

	file2 := filepath.Join(dirSub, "file2")
	data2 := make([]byte, 32*1024)
	if err := os.WriteFile(file2, data2, 0o644); err != nil {
		t.Fatal(err)
	}

	// Set explicit mtime/atime on file2 to verify timestamp propagation.
	future := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	past := time.Now().Add(-365 * 24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(file1, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file2, future, future); err != nil {
		t.Fatal(err)
	}

	dirMap := walkAll(tmp, runtime.NumCPU(), true)
	if len(dirMap) != 4 {
		t.Fatalf("expected 4 directories in dirMap, got %d", len(dirMap))
	}

	root := buildTree(tmp, dirMap)
	if root == nil {
		t.Fatal("root is nil")
	}

	// Hardlink should be deduplicated: 2 unique files (file1 + file2), total 96 KiB apparent size.
	if root.fileCount != 2 {
		t.Errorf("expected root.fileCount = 2 (hardlink deduplicated), got %d", root.fileCount)
	}
	wantSize := int64(len(data1) + len(data2))
	if root.totalSize != wantSize {
		t.Errorf("expected root.totalSize = %d, got %d", wantSize, root.totalSize)
	}
	if !root.newestTime.Equal(future) {
		t.Errorf("expected root.newestTime = %v, got %v", future, root.newestTime)
	}

	// Empty directory should have zero time and 0 size.
	emptyNode := dirMap[dirEmpty]
	if emptyNode == nil {
		t.Fatal("missing empty dir node")
	}
	if !emptyNode.newestTime.IsZero() {
		t.Errorf("expected empty dir newestTime to be zero, got %v", emptyNode.newestTime)
	}
}
