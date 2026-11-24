//go:build linux

package main

import (
	"syscall"
	"time"
	"unsafe"
)

// scanBufSize is the per-worker buffer size for getdents64.
const scanBufSize = 64 * 1024

const (
	atSymlinkNofollow = 0x100
	atNoAutomount     = 0x800
)

func nameOffset() int {
	var d syscall.Dirent
	return int(unsafe.Offsetof(d.Name))
}

func scanDir(dirPath string, buf []byte, seen *inodeTracker, apparentSize bool, minFileBytes int64, exclude *excludeMatcher) (ownSize, ownFiles int64, newestTime time.Time, subdirs, emptyDirs []string, largeFiles []fileEntry, err error) {
	fd, err := syscall.Open(dirPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, time.Time{}, nil, nil, nil, err
	}
	defer syscall.Close(fd)

	var maxSec, maxNsec int64
	nameOff := nameOffset()

	// Scratch buffer for NUL-terminated filename passed to fstatat.
	var nameBuf [256]byte

	for {
		n, err := syscall.Getdents(fd, buf)
		if err != nil {
			return ownSize, ownFiles, newestTime, subdirs, nil, largeFiles, err
		}
		if n <= 0 {
			break
		}

		offset := 0
		for offset < n {
			dirent := (*syscall.Dirent)(unsafe.Pointer(&buf[offset]))
			reclen := int(dirent.Reclen)
			if reclen <= 0 || offset+reclen > n {
				break
			}

			nameBytes := buf[offset+nameOff : offset+reclen]
			offset += reclen

			nameLen := 0
			for nameLen < len(nameBytes) && nameBytes[nameLen] != 0 {
				nameLen++
			}
			if nameLen == 0 {
				continue
			}
			if nameBytes[0] == '.' && (nameLen == 1 || (nameLen == 2 && nameBytes[1] == '.')) {
				continue
			}

			dtype := dirent.Type

			if dtype == syscall.DT_DIR {
				subPath := joinPath(dirPath, string(nameBytes[:nameLen]))
				if exclude != nil && exclude.match(subPath) {
					continue
				}
				subdirs = append(subdirs, subPath)
				continue
			}

			// Prepare NUL-terminated pointer for raw fstatat syscall without heap allocation.
			var namePtr *byte
			if nameLen < len(nameBuf) {
				copy(nameBuf[:], nameBytes[:nameLen])
				nameBuf[nameLen] = 0
				namePtr = &nameBuf[0]
			} else if nameLen < len(nameBytes) && nameBytes[nameLen] == 0 {
				namePtr = &nameBytes[0]
			} else {
				b := make([]byte, nameLen+1)
				copy(b, nameBytes[:nameLen])
				namePtr = &b[0]
			}

			var st syscall.Stat_t
			_, _, errno := syscall.Syscall6(
				fstatatTrap,
				uintptr(fd),
				uintptr(unsafe.Pointer(namePtr)),
				uintptr(unsafe.Pointer(&st)),
				uintptr(atSymlinkNofollow|atNoAutomount),
				0,
				0,
			)
			if errno != 0 {
				continue
			}

			// Handle filesystems that return DT_UNKNOWN (e.g., XFS without ftype, NFS, FUSE).
			if dtype == syscall.DT_UNKNOWN && (st.Mode&syscall.S_IFMT) == syscall.S_IFDIR {
				subPath := joinPath(dirPath, string(nameBytes[:nameLen]))
				if exclude != nil && exclude.match(subPath) {
					continue
				}
				subdirs = append(subdirs, subPath)
				continue
			}

			if exclude != nil && exclude.match(joinPath(dirPath, string(nameBytes[:nameLen]))) {
				continue
			}

			// Deduplicate hardlinks (files with Nlink > 1).
			if st.Nlink > 1 && st.Ino != 0 && seen != nil {
				if !seen.add(uint64(st.Ino)) {
					continue
				}
			}

			var fileSize int64
			if apparentSize {
				fileSize = st.Size
			} else {
				fileSize = st.Blocks * 512
			}
			ownSize += fileSize
			ownFiles++

			// Use the newer of atime and mtime so noatime/relatime mounts still reflect recent activity.
			sec, nsec := int64(st.Atim.Sec), int64(st.Atim.Nsec)
			msec, mnsec := int64(st.Mtim.Sec), int64(st.Mtim.Nsec)
			if msec > sec || (msec == sec && mnsec > nsec) {
				sec, nsec = msec, mnsec
			}
			if sec > maxSec || (sec == maxSec && nsec > maxNsec) {
				maxSec, maxNsec = sec, nsec
			}

			if minFileBytes > 0 && fileSize >= minFileBytes {
				var ft time.Time
				if sec > 0 || nsec > 0 {
					ft = time.Unix(sec, nsec)
				}
				largeFiles = append(largeFiles, fileEntry{
					path:    joinPath(dirPath, string(nameBytes[:nameLen])),
					size:    fileSize,
					modTime: ft,
				})
			}
		}
	}

	if maxSec > 0 || maxNsec > 0 {
		newestTime = time.Unix(maxSec, maxNsec)
	}
	return ownSize, ownFiles, newestTime, subdirs, nil, largeFiles, nil
}
