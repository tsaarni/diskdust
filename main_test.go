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

	dirMap := walkAll(tmp, runtime.NumCPU(), true, 0, nil, nil)
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

func TestCollapseAndLargeFiles(t *testing.T) {
	tmp := t.TempDir()

	// Chain: chain/a/b/big.bin (100 KiB) -> chain/, chain/a/, chain/a/b/ should all collapse to chain/a/b/big.bin.
	chainLeaf := filepath.Join(tmp, "chain", "a", "b")
	if err := os.MkdirAll(chainLeaf, 0o755); err != nil {
		t.Fatal(err)
	}
	bigFile := filepath.Join(chainLeaf, "big.bin")
	if err := os.WriteFile(bigFile, make([]byte, 100*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mixed dir: mixed/ has two 40 KiB files -> neither is >=95% of 80 KiB,
	// so mixed/ AND both files (>= 30 KiB threshold) should be emitted.
	mixedDir := filepath.Join(tmp, "mixed")
	if err := os.MkdirAll(mixedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixedDir, "f1.bin"), make([]byte, 40*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixedDir, "f2.bin"), make([]byte, 40*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	minBytes := int64(30 * 1024)
	dirMap := walkAll(tmp, runtime.NumCPU(), true, minBytes, nil, nil)
	root := buildTree(tmp, dirMap)
	results := collectResults(root, minBytes, time.Time{})

	got := make(map[string]bool)
	for _, r := range results {
		rel, _ := filepath.Rel(tmp, r.path)
		if r.isDir {
			rel += "/"
		}
		got[rel] = true
	}

	// Ancestor wrapper dirs should be collapsed away.
	for _, collapsed := range []string{"chain/", "chain/a/", "chain/a/b/"} {
		if got[collapsed] {
			t.Errorf("expected %q to be collapsed, but it was listed (got=%v)", collapsed, got)
		}
	}
	// Expected entries: chain/a/b/big.bin, mixed/, mixed/f1.bin, mixed/f2.bin.
	for _, want := range []string{"chain/a/b/big.bin", "mixed/", "mixed/f1.bin", "mixed/f2.bin"} {
		if !got[want] {
			t.Errorf("expected %q in results, got=%v", want, got)
		}
	}
}

func TestExclude(t *testing.T) {
	tmp := t.TempDir()

	keepDir := filepath.Join(tmp, "keep")
	skipDir := filepath.Join(tmp, "skip", "nested")
	gitDir := filepath.Join(tmp, "keep", ".git")
	for _, d := range []string{keepDir, skipDir, gitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(keepDir, "a.txt"), make([]byte, 10*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "b.dmg"), make([]byte, 20*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skipDir, "c.bin"), make([]byte, 50*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "pack"), make([]byte, 30*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	ex := newExcludeMatcher(tmp, []string{"skip", ".git", "*.dmg"})
	dirMap := walkAll(tmp, runtime.NumCPU(), true, 0, ex, nil)
	root := buildTree(tmp, dirMap)

	if root.totalSize != 10*1024 {
		t.Errorf("expected totalSize = %d after excludes, got %d", 10*1024, root.totalSize)
	}
	if _, exists := dirMap[filepath.Join(tmp, "skip")]; exists {
		t.Errorf("expected excluded dir 'skip' not to be walked")
	}
	if _, exists := dirMap[gitDir]; exists {
		t.Errorf("expected excluded dir '.git' not to be walked")
	}
}

func TestDustScore(t *testing.T) {
	now := time.Now()
	const gib = 1024 * 1024 * 1024

	// 10 GiB untouched for 270 days should outrank 100 GiB touched 1 day ago,
	// 500 MiB untouched for 1000 days, and a 2.6 GiB file with a bogus 30-year-old timestamp.
	staleBig := dustScore(10*gib, now.AddDate(0, 0, -270), now)
	activeHuge := dustScore(100*gib, now.AddDate(0, 0, -1), now)
	ancientSmall := dustScore(512*1024*1024, now.AddDate(0, 0, -1000), now)
	bogusOld := dustScore(26*gib/10, now.AddDate(-30, 0, 0), now)

	if !(staleBig > activeHuge) {
		t.Errorf("expected staleBig (%v) > activeHuge (%v)", staleBig, activeHuge)
	}
	if !(staleBig > ancientSmall) {
		t.Errorf("expected staleBig (%v) > ancientSmall (%v)", staleBig, ancientSmall)
	}
	if !(staleBig > bogusOld) {
		t.Errorf("expected staleBig (%v) > bogusOld (%v)", staleBig, bogusOld)
	}
	if dustScore(10*gib, time.Time{}, now) != 0 {
		t.Errorf("expected zero time to have dustScore 0")
	}
}
