package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// dirNode is a node in the directory tree with aggregated stats.
type dirNode struct {
	name       string
	path       string
	ownSize    int64     // size of files directly in this directory
	ownFiles   int64     // count of files directly in this directory
	ownTime    time.Time // newest access/mod time of files directly in this directory
	totalSize  int64     // ownSize + all descendants
	newestTime time.Time // newest access/mod time across all descendants
	fileCount  int64     // ownFiles + all descendants
	children   []*dirNode
}

// inodeTracker tracks hard-linked file inodes across workers using sharded maps
// to avoid lock contention when deduplicating hardlinks.
type inodeTracker struct {
	shards [64]struct {
		mu  sync.Mutex
		set map[uint64]struct{}
	}
}

func newInodeTracker() *inodeTracker {
	t := &inodeTracker{}
	for i := range t.shards {
		t.shards[i].set = make(map[uint64]struct{})
	}
	return t
}

// add records ino and returns true if this is the first time seeing it.
func (t *inodeTracker) add(ino uint64) bool {
	// Fibonacci hashing for shard selection.
	idx := (ino * 11400714819323198485) >> 58
	s := &t.shards[idx]
	s.mu.Lock()
	_, exists := s.set[ino]
	if !exists {
		s.set[ino] = struct{}{}
	}
	s.mu.Unlock()
	return !exists
}

var (
	notAccessed  int
	topN         int
	workers      int
	minSizeStr   string
	sortBy       string
	apparentSize bool
)

func main() {
	cmd := &cobra.Command{
		Use:   "diskdust [path]",
		Short: "Find the largest directories on your disk",
		Long:  "Parallel filesystem walker that finds the largest directories. Optionally filter by last access time.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  run,
	}
	cmd.Flags().IntVar(&notAccessed, "not-accessed", 0, "only show directories not accessed in this many days (0 = show all)")
	cmd.Flags().IntVar(&topN, "top", 40, "number of results to show")
	cmd.Flags().IntVar(&workers, "workers", runtime.NumCPU(), "number of parallel workers")
	cmd.Flags().StringVar(&minSizeStr, "min-size", "500MB", "minimum size to display (e.g. 100MB, 1GB)")
	cmd.Flags().StringVar(&sortBy, "sort", "size", "sort order: size or accessed")
	cmd.Flags().BoolVar(&apparentSize, "apparent-size", false, "use apparent file size instead of actual disk block usage")
	cmd.SilenceUsage = true

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	target := os.Getenv("HOME")
	if len(args) > 0 {
		target = args[0]
	}

	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("bad path: %w", err)
	}
	target = abs

	minBytesU, err := humanize.ParseBytes(minSizeStr)
	if err != nil {
		return fmt.Errorf("bad min-size: %w", err)
	}
	minBytes := int64(minBytesU)

	if sortBy != "size" && sortBy != "accessed" {
		return fmt.Errorf("bad sort: %q (must be \"size\" or \"accessed\")", sortBy)
	}

	var cutoff time.Time
	if notAccessed > 0 {
		cutoff = time.Now().AddDate(0, 0, -notAccessed)
	}

	// Print scan info immediately.
	fmt.Printf("Scanning: %s\n", target)
	fmt.Printf("Filters:  min-size=%s  top=%d", minSizeStr, topN)
	if notAccessed > 0 {
		fmt.Printf("  not-accessed=%d (cutoff=%s)", notAccessed, cutoff.Format("2006-01-02"))
	}
	fmt.Printf("\n")

	// Phase 1: walk and collect per-directory stats.
	start := time.Now()
	dirMap := walkAll(target, workers, apparentSize)
	elapsed := time.Since(start)

	// Phase 2: build tree from flat map.
	root := buildTree(target, dirMap)
	if root == nil {
		return fmt.Errorf("no data found")
	}

	fmt.Printf("\n")

	// Phase 3: flatten tree, filter, sort, take top N.
	var all []*dirNode
	var collect func(n *dirNode)
	collect = func(n *dirNode) {
		for _, c := range n.children {
			if c.totalSize >= minBytes {
				if cutoff.IsZero() || (!c.newestTime.IsZero() && c.newestTime.Before(cutoff)) {
					all = append(all, c)
				}
			}
			collect(c)
		}
	}
	collect(root)

	sort.Slice(all, func(i, j int) bool {
		switch sortBy {
		case "accessed":
			return all[i].newestTime.Before(all[j].newestTime)
		default:
			return all[i].totalSize > all[j].totalSize
		}
	})
	if len(all) > topN {
		all = all[:topN]
	}

	// Compute listed size without double-counting subdirectories whose ancestor is also in `all`.
	listedSet := make(map[string]struct{}, len(all))
	for _, n := range all {
		listedSet[n.path] = struct{}{}
	}
	var listedSize int64
	for _, n := range all {
		hasListedAncestor := false
		for p := filepath.Dir(n.path); len(p) >= len(target) && p != "." && p != "/"; p = filepath.Dir(p) {
			if _, ok := listedSet[p]; ok {
				hasListedAncestor = true
				break
			}
			if p == target {
				break
			}
		}
		if !hasListedAncestor {
			listedSize += n.totalSize
		}
	}

	// Phase 4: print results.
	fmt.Printf("%9s  %10s  %9s  %s\n", "SIZE", "ACCESSED", "FILES", "PATH")

	for _, n := range all {
		rel, _ := filepath.Rel(target, n.path)
		accessedStr := "-"
		if !n.newestTime.IsZero() {
			accessedStr = n.newestTime.Format("2006-01-02")
		}
		fmt.Printf("%9s  %10s  %9d  %s/\n",
			humanize.IBytes(uint64(n.totalSize)),
			accessedStr,
			n.fileCount, rel)
	}

	fmt.Printf("\nScanned %s files in %s directories (%s) in %s\n",
		humanize.Comma(root.fileCount),
		humanize.Comma(int64(len(dirMap))),
		humanize.IBytes(uint64(root.totalSize)),
		elapsed.Round(time.Millisecond))
	fmt.Printf("Listed %d directories (%s)\n",
		len(all),
		humanize.IBytes(uint64(listedSize)))

	return nil
}

// buildTree constructs a directory tree from the flat per-directory stats map.
func buildTree(root string, dirMap map[string]*dirNode) *dirNode {
	rootNode, ok := dirMap[root]
	if !ok {
		rootNode = &dirNode{name: filepath.Base(root), path: root}
		dirMap[root] = rootNode
	}

	// Link children to parents.
	for path, node := range dirMap {
		if path == root {
			continue
		}
		parentPath := filepath.Dir(path)
		parent, ok := dirMap[parentPath]
		if !ok {
			continue
		}
		parent.children = append(parent.children, node)
	}

	// Propagate sizes up: children stats bubble up to parents.
	propagate(rootNode)

	return rootNode
}

// propagate recursively computes total size, file count, and newest time
// by summing own stats plus all children.
func propagate(n *dirNode) {
	n.totalSize = n.ownSize
	n.fileCount = n.ownFiles
	n.newestTime = n.ownTime
	for _, c := range n.children {
		propagate(c)
		n.totalSize += c.totalSize
		n.fileCount += c.fileCount
		if c.newestTime.After(n.newestTime) {
			n.newestTime = c.newestTime
		}
	}
}

// walkAll walks the entire directory tree and returns per-directory stats.
// Each directory entry contains only the files directly in that directory
// (not recursively summed — that happens in propagate).
func walkAll(target string, numWorkers int, apparentSize bool) map[string]*dirNode {
	if numWorkers < 1 {
		numWorkers = 1
	}

	var (
		mu       sync.Mutex
		result   = make(map[string]*dirNode, 4096)
		wg       sync.WaitGroup
		ch       = make(chan string, 8192)
		inodes   = newInodeTracker()
		workerWg sync.WaitGroup
	)

	worker := func() {
		defer workerWg.Done()

		// Per-worker reusable syscall buffer and local node accumulator.
		buf := make([]byte, scanBufSize)
		localNodes := make([]*dirNode, 0, 512)
		var localQueue []string

		flush := func() {
			if len(localNodes) == 0 {
				return
			}
			mu.Lock()
			for _, n := range localNodes {
				result[n.path] = n
			}
			mu.Unlock()
			localNodes = localNodes[:0]
		}

		processOne := func(dir string) {
			ownSize, ownFiles, newestTime, subdirs, err := scanDir(dir, buf, inodes, apparentSize)
			if err != nil && ownFiles == 0 && len(subdirs) == 0 && dir != target {
				return
			}

			localNodes = append(localNodes, &dirNode{
				name:     filepath.Base(dir),
				path:     dir,
				ownSize:  ownSize,
				ownFiles: ownFiles,
				ownTime:  newestTime,
			})
			if len(localNodes) >= 512 {
				flush()
			}

			for _, sub := range subdirs {
				wg.Add(1)
				select {
				case ch <- sub:
				default:
					wg.Done()
					localQueue = append(localQueue, sub)
				}
			}
		}

		for dir := range ch {
			processOne(dir)
			for len(localQueue) > 0 {
				// Pop from end (LIFO) for bounded memory and cache locality,
				// while opportunistically feeding idle workers via ch in processOne.
				idx := len(localQueue) - 1
				next := localQueue[idx]
				localQueue[idx] = ""
				localQueue = localQueue[:idx]
				processOne(next)
			}
			wg.Done()
		}

		flush()
	}

	for i := 0; i < numWorkers; i++ {
		workerWg.Add(1)
		go worker()
	}

	wg.Add(1)
	ch <- target

	wg.Wait()
	close(ch)
	workerWg.Wait()

	return result
}
