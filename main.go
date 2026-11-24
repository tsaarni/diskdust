package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// fileEntry represents an individual large file (>= min-size) found during the walk.
type fileEntry struct {
	path    string
	size    int64
	modTime time.Time // newer of atime and mtime
}

// dirNode is a node in the directory tree with aggregated stats.
type dirNode struct {
	name       string
	path       string
	ownSize    int64       // size of files directly in this directory
	ownFiles   int64       // count of files directly in this directory
	ownTime    time.Time   // newest access/mod time of files directly in this directory
	largeFiles []fileEntry // individual files directly in this dir with size >= minFileBytes
	totalSize  int64       // ownSize + all descendants
	newestTime time.Time   // newest access/mod time across all descendants
	fileCount  int64       // ownFiles + all descendants
	children   []*dirNode
}

// resultEntry is a directory or large individual file shown in the output table.
type resultEntry struct {
	path       string
	size       int64
	newestTime time.Time
	fileCount  int64
	isDir      bool
}

// excludeMatcher matches paths against user-provided --exclude patterns.
type excludeMatcher struct {
	target   string
	patterns []string
}

func newExcludeMatcher(target string, patterns []string) *excludeMatcher {
	cleaned := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p != "/" {
			p = strings.TrimRight(p, "/")
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return nil
	}
	return &excludeMatcher{target: target, patterns: cleaned}
}

func (m *excludeMatcher) match(fullPath string) bool {
	if m == nil {
		return false
	}
	var rel string
	if m.target == "/" {
		rel = strings.TrimPrefix(fullPath, "/")
	} else if strings.HasPrefix(fullPath, m.target+"/") {
		rel = fullPath[len(m.target)+1:]
	} else if fullPath == m.target {
		rel = "."
	} else {
		rel = fullPath
	}
	base := filepath.Base(fullPath)

	for _, p := range m.patterns {
		if strings.HasPrefix(p, "/") {
			if fullPath == p {
				return true
			}
			if ok, _ := filepath.Match(p, fullPath); ok {
				return true
			}
		} else if strings.Contains(p, "/") {
			if rel == p {
				return true
			}
			if ok, _ := filepath.Match(p, rel); ok {
				return true
			}
		} else {
			if base == p {
				return true
			}
			if ok, _ := filepath.Match(p, base); ok {
				return true
			}
		}
	}
	return false
}

// scanProgress tracks live scan counts across workers for stderr progress output.
type scanProgress struct {
	files atomic.Int64
	dirs  atomic.Int64
	bytes atomic.Int64
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
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
	excludes     []string
)

// collapseThresholdPct is the percentage of a directory's size that a single
// qualifying child (directory or file) must reach for the parent to be collapsed.
const collapseThresholdPct = 95

func main() {
	cmd := &cobra.Command{
		Use:   "diskdust [path]",
		Short: "Find the largest directories on your disk",
		Long:  "Parallel filesystem walker that finds the largest directories and files. Optionally filter by last access time.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  run,
	}
	cmd.Flags().IntVar(&notAccessed, "not-accessed", 0, "only show entries not accessed in this many days (0 = show all)")
	cmd.Flags().IntVar(&topN, "top", 40, "number of results to show")
	cmd.Flags().IntVar(&workers, "workers", runtime.NumCPU(), "number of parallel workers")
	cmd.Flags().StringVar(&minSizeStr, "min-size", "500MB", "minimum size to display (e.g. 100MB, 1GB)")
	cmd.Flags().StringVar(&sortBy, "sort", "size", "sort order: size, accessed, or dust")
	cmd.Flags().BoolVar(&apparentSize, "apparent-size", false, "use apparent file size instead of actual disk block usage")
	cmd.Flags().StringArrayVar(&excludes, "exclude", nil, "exclude paths matching pattern (repeatable, e.g. Downloads/foo, .git, *.dmg)")
	cmd.SilenceUsage = true

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func matchesCutoff(t, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return true
	}
	return !t.IsZero() && t.Before(cutoff)
}

// dustScore ranks entries by both size and age using a saturating age weight:
// size * (ageDays / (ageDays + 365)). Active entries score near 0, 1-year-old
// entries reach 50% of their size, and bogus historical timestamps (e.g. 1996
// zip dates) asymptotically approach 100% instead of exploding the score.
func dustScore(size int64, newestTime, now time.Time) float64 {
	if size <= 0 || newestTime.IsZero() {
		return 0
	}
	ageDays := now.Sub(newestTime).Hours() / 24.0
	if ageDays <= 0 {
		return 0
	}
	return float64(size) * (ageDays / (ageDays + 365.0))
}

// collectResults flattens the tree into displayable directories and large files,
// collapsing any directory whose size is >= collapseThresholdPct accounted for
// by a single qualifying child directory or file.
func collectResults(root *dirNode, minBytes int64, cutoff time.Time) []resultEntry {
	var all []resultEntry

	// Emit large loose files directly in the root directory.
	for _, f := range root.largeFiles {
		if f.size >= minBytes && matchesCutoff(f.modTime, cutoff) {
			all = append(all, resultEntry{
				path:       f.path,
				size:       f.size,
				newestTime: f.modTime,
				fileCount:  1,
				isDir:      false,
			})
		}
	}

	var collect func(n *dirNode)
	collect = func(n *dirNode) {
		for _, c := range n.children {
			if c.totalSize >= minBytes && matchesCutoff(c.newestTime, cutoff) {
				redundant := false
				for _, gc := range c.children {
					if gc.totalSize >= minBytes && matchesCutoff(gc.newestTime, cutoff) &&
						gc.totalSize*100 >= c.totalSize*collapseThresholdPct {
						redundant = true
						break
					}
				}
				if !redundant {
					for _, f := range c.largeFiles {
						if f.size >= minBytes && matchesCutoff(f.modTime, cutoff) &&
							f.size*100 >= c.totalSize*collapseThresholdPct {
							redundant = true
							break
						}
					}
				}
				if !redundant {
					all = append(all, resultEntry{
						path:       c.path,
						size:       c.totalSize,
						newestTime: c.newestTime,
						fileCount:  c.fileCount,
						isDir:      true,
					})
				}
			}

			for _, f := range c.largeFiles {
				if f.size >= minBytes && matchesCutoff(f.modTime, cutoff) {
					all = append(all, resultEntry{
						path:       f.path,
						size:       f.size,
						newestTime: f.modTime,
						fileCount:  1,
						isDir:      false,
					})
				}
			}

			collect(c)
		}
	}
	collect(root)

	return all
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

	if sortBy != "size" && sortBy != "accessed" && sortBy != "dust" {
		return fmt.Errorf("bad sort: %q (must be \"size\", \"accessed\", or \"dust\")", sortBy)
	}

	now := time.Now()
	var cutoff time.Time
	if notAccessed > 0 {
		cutoff = now.AddDate(0, 0, -notAccessed)
	}

	exclude := newExcludeMatcher(target, excludes)

	// Print scan info immediately.
	fmt.Printf("Scanning: %s\n", target)
	fmt.Printf("Filters:  min-size=%s  top=%d", minSizeStr, topN)
	if sortBy != "size" {
		fmt.Printf("  sort=%s", sortBy)
	}
	if notAccessed > 0 {
		fmt.Printf("  not-accessed=%d (cutoff=%s)", notAccessed, cutoff.Format("2006-01-02"))
	}
	if exclude != nil {
		fmt.Printf("  exclude=%s", strings.Join(exclude.patterns, ","))
	}
	fmt.Printf("\n")

	// Start stderr progress ticker if stderr is a terminal.
	var progress *scanProgress
	var stopProgress func()
	if isTerminal(os.Stderr) {
		progress = &scanProgress{}
		done := make(chan struct{})
		var pwg sync.WaitGroup
		pwg.Add(1)
		startTime := time.Now()
		go func() {
			defer pwg.Done()
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					fmt.Fprintf(os.Stderr, "\r\033[K")
					return
				case <-ticker.C:
					fmt.Fprintf(os.Stderr, "\rScanning... %s files in %s dirs (%s) [%ds]\033[K",
						humanize.Comma(progress.files.Load()),
						humanize.Comma(progress.dirs.Load()),
						humanize.IBytes(uint64(progress.bytes.Load())),
						int(time.Since(startTime).Seconds()))
				}
			}
		}()
		stopProgress = func() {
			close(done)
			pwg.Wait()
		}
	}

	// Phase 1: walk and collect per-directory stats (and individual files >= minBytes).
	start := time.Now()
	dirMap := walkAll(target, workers, apparentSize, minBytes, exclude, progress)
	elapsed := time.Since(start)
	if stopProgress != nil {
		stopProgress()
	}

	// Phase 2: build tree from flat map.
	root := buildTree(target, dirMap)
	if root == nil {
		return fmt.Errorf("no data found")
	}

	fmt.Printf("\n")

	// Phase 3: flatten tree, collapse redundant ancestors, filter, sort, take top N.
	all := collectResults(root, minBytes, cutoff)

	sort.Slice(all, func(i, j int) bool {
		switch sortBy {
		case "accessed":
			ti, tj := all[i].newestTime, all[j].newestTime
			if ti.IsZero() != tj.IsZero() {
				return !ti.IsZero()
			}
			return ti.Before(tj)
		case "dust":
			si := dustScore(all[i].size, all[i].newestTime, now)
			sj := dustScore(all[j].size, all[j].newestTime, now)
			if math.Abs(si-sj) > 1e-9 {
				return si > sj
			}
			return all[i].size > all[j].size
		default:
			return all[i].size > all[j].size
		}
	})
	if len(all) > topN {
		all = all[:topN]
	}

	// Compute listed size without double-counting entries whose ancestor is also in `all`.
	listedSet := make(map[string]struct{}, len(all))
	for _, e := range all {
		if e.isDir {
			listedSet[e.path] = struct{}{}
		}
	}
	var listedSize int64
	for _, e := range all {
		hasListedAncestor := false
		for p := filepath.Dir(e.path); len(p) >= len(target) && p != "." && p != "/"; p = filepath.Dir(p) {
			if _, ok := listedSet[p]; ok {
				hasListedAncestor = true
				break
			}
			if p == target {
				break
			}
		}
		if !hasListedAncestor {
			listedSize += e.size
		}
	}

	// Phase 4: print results.
	fmt.Printf("%9s  %10s  %9s  %s\n", "SIZE", "ACCESSED", "FILES", "PATH")

	for _, e := range all {
		rel, _ := filepath.Rel(target, e.path)
		accessedStr := "-"
		if !e.newestTime.IsZero() {
			accessedStr = e.newestTime.Format("2006-01-02")
		}
		suffix := ""
		if e.isDir {
			suffix = "/"
		}
		fmt.Printf("%9s  %10s  %9d  %s%s\n",
			humanize.IBytes(uint64(e.size)),
			accessedStr,
			e.fileCount, rel, suffix)
	}

	fmt.Printf("\nScanned %s files in %s directories (%s) in %s\n",
		humanize.Comma(root.fileCount),
		humanize.Comma(int64(len(dirMap))),
		humanize.IBytes(uint64(root.totalSize)),
		elapsed.Round(time.Millisecond))
	fmt.Printf("Listed %d entries (%s)\n",
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
func walkAll(target string, numWorkers int, apparentSize bool, minFileBytes int64, exclude *excludeMatcher, progress *scanProgress) map[string]*dirNode {
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
		var pDirs, pFiles, pBytes int64

		flushProgress := func() {
			if progress == nil || pDirs == 0 {
				return
			}
			progress.dirs.Add(pDirs)
			progress.files.Add(pFiles)
			progress.bytes.Add(pBytes)
			pDirs, pFiles, pBytes = 0, 0, 0
		}

		flush := func() {
			flushProgress()
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
			ownSize, ownFiles, newestTime, subdirs, largeFiles, err := scanDir(dir, buf, inodes, apparentSize, minFileBytes, exclude)
			if err != nil && ownFiles == 0 && len(subdirs) == 0 && dir != target {
				return
			}

			localNodes = append(localNodes, &dirNode{
				name:       filepath.Base(dir),
				path:       dir,
				ownSize:    ownSize,
				ownFiles:   ownFiles,
				ownTime:    newestTime,
				largeFiles: largeFiles,
			})
			if progress != nil {
				pDirs++
				pFiles += ownFiles
				pBytes += ownSize
				if pDirs >= 64 {
					flushProgress()
				}
			}
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
