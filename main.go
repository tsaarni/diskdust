package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// dirNode is a node in the directory tree with aggregated stats.
type dirNode struct {
	name       string
	path       string
	ownSize    int64     // size of files directly in this directory
	ownFiles   int64     // count of files directly in this directory
	ownTime    time.Time // newest atime of files directly in this directory
	totalSize  int64     // ownSize + all descendants
	newestTime time.Time // newest atime across all descendants
	fileCount  int64     // ownFiles + all descendants
	children   []*dirNode
}

var (
	notAccessed int
	topN        int
	workers     int
	minSizeStr  string
	sortBy      string
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
	dirMap := walkAll(target, workers)
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
				if cutoff.IsZero() || c.newestTime.Before(cutoff) {
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
	matched := len(all)
	if len(all) > topN {
		all = all[:topN]
	}

	var listedSize int64
	for _, n := range all {
		listedSize += n.totalSize
	}

	// Phase 4: print results.
	fmt.Printf("%9s  %10s  %9s  %s\n", "SIZE", "ACCESSED", "FILES", "PATH")

	for _, n := range all {
		rel, _ := filepath.Rel(target, n.path)
		fmt.Printf("%9s  %10s  %9d  %s/\n",
			humanize.IBytes(uint64(n.totalSize)),
			n.newestTime.Format("2006-01-02"),
			n.fileCount, rel)
	}

	fmt.Printf("\nScanned %s files in %s directories (%s) in %s\n",
		humanize.Comma(root.fileCount),
		humanize.Comma(int64(matched)),
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
func walkAll(target string, numWorkers int) map[string]*dirNode {
	var (
		mu     sync.Mutex
		result = make(map[string]*dirNode)
		wg     sync.WaitGroup
		ch     = make(chan string, 4096)
	)

	type dirEntry struct {
		name  string
		isDir bool
	}

	readDir := func(dirPath string) ([]dirEntry, error) {
		fd, err := syscall.Open(dirPath, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
		defer syscall.Close(fd)

		var entries []dirEntry
		buf := make([]byte, 32768)

		for {
			n, err := syscall.Getdents(fd, buf)
			if err != nil {
				return entries, err
			}
			if n <= 0 {
				break
			}

			offset := 0
			for offset < n {
				dirent := (*syscall.Dirent)(unsafe.Pointer(&buf[offset]))
				nameBytes := buf[offset+nameOffset() : offset+int(dirent.Reclen)]
				nameLen := 0
				for nameLen < len(nameBytes) && nameBytes[nameLen] != 0 {
					nameLen++
				}
				name := string(nameBytes[:nameLen])
				offset += int(dirent.Reclen)

				if name == "." || name == ".." {
					continue
				}

				entries = append(entries, dirEntry{name: name, isDir: dirent.Type == syscall.DT_DIR})
			}
		}
		return entries, nil
	}

	var processDir func(dir string, localResult map[string]*dirNode)

	processDir = func(dir string, localResult map[string]*dirNode) {
		entries, err := readDir(dir)
		if err != nil {
			return
		}

		node, ok := localResult[dir]
		if !ok {
			node = &dirNode{name: filepath.Base(dir), path: dir}
			localResult[dir] = node
		}

		for _, entry := range entries {
			fullPath := dir + "/" + entry.name

			if entry.isDir {
				// Ensure child dir exists in local map so tree building works.
				if _, ok := localResult[fullPath]; !ok {
					localResult[fullPath] = &dirNode{name: entry.name, path: fullPath}
				}
				wg.Add(1)
				select {
				case ch <- fullPath:
				default:
					wg.Done()
					processDir(fullPath, localResult)
				}
				continue
			}

			var st syscall.Stat_t
			if syscall.Lstat(fullPath, &st) != nil {
				continue
			}

			atime := time.Unix(st.Atim.Sec, st.Atim.Nsec)
			node.ownSize += st.Size
			node.ownFiles++
			if atime.After(node.ownTime) {
				node.ownTime = atime
			}
		}
	}

	flushLocal := func(localResult map[string]*dirNode) {
		if len(localResult) == 0 {
			return
		}
		mu.Lock()
		for path, ln := range localResult {
			n, ok := result[path]
			if !ok {
				result[path] = &dirNode{
					name:    ln.name,
					path:    ln.path,
					ownSize: ln.ownSize,
					ownFiles: ln.ownFiles,
					ownTime: ln.ownTime,
				}
			} else {
				n.ownSize += ln.ownSize
				n.ownFiles += ln.ownFiles
				if ln.ownTime.After(n.ownTime) {
					n.ownTime = ln.ownTime
				}
			}
		}
		mu.Unlock()
	}

	var workerWg sync.WaitGroup

	worker := func() {
		localResult := make(map[string]*dirNode)
		flushCount := 0

		for dir := range ch {
			processDir(dir, localResult)
			flushCount++
			if flushCount >= 128 {
				flushLocal(localResult)
				localResult = make(map[string]*dirNode)
				flushCount = 0
			}
			wg.Done()
		}
		flushLocal(localResult)
		workerWg.Done()
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

func nameOffset() int {
	var d syscall.Dirent
	return int(unsafe.Offsetof(d.Name))
}
