//go:build darwin

package main

import (
	"syscall"
	"time"
	"unsafe"
)

//go:cgo_import_dynamic libc_getattrlistbulk getattrlistbulk "/usr/lib/libSystem.B.dylib"

var libc_getattrlistbulk_trampoline_addr uintptr

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// attrList corresponds to struct attrlist in <sys/attr.h>.
type attrList struct {
	bitmapcount uint16
	reserved    uint16
	commonattr  uint32
	volattr     uint32
	dirattr     uint32
	fileattr    uint32
	forkattr    uint32
}

// attributeSet corresponds to attribute_set_t in <sys/attr.h>.
type attributeSet struct {
	commonattr uint32
	volattr    uint32
	dirattr    uint32
	fileattr   uint32
	forkattr   uint32
}

// attrReference corresponds to attrreference_t in <sys/attr.h>.
type attrReference struct {
	dataOffset int32
	length     uint32
}

const (
	attrBitMapCount = 5

	// Common attributes (commonattr)
	attrCmnName          = 0x00000001
	attrCmnDevID         = 0x00000002
	attrCmnObjType       = 0x00000008
	attrCmnModTime       = 0x00000400
	attrCmnAccTime       = 0x00001000
	attrCmnFlags         = 0x00040000
	attrCmnFileID        = 0x02000000
	attrCmnError         = 0x20000000
	attrCmnReturnedAttrs = 0x80000000

	// Directory attributes (dirattr)
	attrDirEntryCount  = 0x00000002
	attrDirMountStatus = 0x00000004

	// File attributes (fileattr)
	attrFileLinkCount = 0x00000001
	attrFileTotalSize = 0x00000002
	attrFileAllocSize = 0x00000004

	// Options for getattrlistbulk
	fsoptNoFollow       = 0x00000001
	fsoptNoInMemUpdate  = 0x00000002
	fsoptPackInvalAttrs = 0x00000008

	// Object types (enum vtype in <sys/vnode.h>)
	vtypeReg = 1 // VREG
	vtypeDir = 2 // VDIR

	// File flags (st_flags in <sys/stat.h>)
	sfFirmlink = 0x00800000 // SF_FIRMLINK
)

// scanBufSize is the per-worker buffer size for getattrlistbulk.
// 128 KiB reduces the number of syscalls on large directories significantly.
const scanBufSize = 128 * 1024

// macOS note: walking trees with more directories than kern.maxvnodes (~250k)
// can fill the kernel vnode cache and trigger global vnlru reclamation pauses
// (5-10s stalls across all workers).
//
// How to observe/verify:
//   - Run `sysctl kern.maxvnodes kern.num_vnodes` during or right after a scan;
//     when stalled, kern.num_vnodes is pinned at kern.maxvnodes (100% full).
//   - All workers (and even unrelated commands like `df`) stall simultaneously
//     for the same ~5-10s wall-clock window and resume together.
//
// To minimize vnode pressure and lock contention:
//   - attrDirEntryCount lets us skip open()ing empty directories (entryCount == 0)
//     so they never allocate a vnode.
//   - fsoptNoInMemUpdate reads attributes straight from the APFS catalog without
//     contending on in-memory vnode locks.
var bulkAttrList = attrList{
	bitmapcount: attrBitMapCount,
	commonattr: attrCmnReturnedAttrs |
		attrCmnName |
		attrCmnError |
		attrCmnObjType |
		attrCmnModTime |
		attrCmnAccTime |
		attrCmnFlags |
		attrCmnFileID,
	dirattr: attrDirEntryCount,
	fileattr: attrFileLinkCount |
		attrFileTotalSize |
		attrFileAllocSize,
}

func scanDir(dirPath string, buf []byte, seen *inodeTracker, apparentSize bool, minFileBytes int64, exclude *excludeMatcher) (ownSize, ownFiles int64, newestTime time.Time, subdirs, emptyDirs []string, largeFiles []fileEntry, err error) {
	fd, err := syscall.Open(dirPath, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return 0, 0, time.Time{}, nil, nil, nil, err
	}
	defer syscall.Close(fd)

	al := bulkAttrList
	var maxSec, maxNsec int64

	for {
		r1, _, errno := syscall_syscall6(
			libc_getattrlistbulk_trampoline_addr,
			uintptr(fd),
			uintptr(unsafe.Pointer(&al)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			fsoptNoFollow|fsoptNoInMemUpdate,
			0,
		)
		if errno != 0 {
			return ownSize, ownFiles, newestTime, subdirs, emptyDirs, largeFiles, errno
		}
		count := int(r1)
		if count <= 0 {
			break
		}

		offset := 0
		for i := 0; i < count && offset+4 <= len(buf); i++ {
			entryLen := int(*(*uint32)(unsafe.Pointer(&buf[offset])))
			if entryLen <= 0 || offset+entryLen > len(buf) {
				break
			}

			pos := offset + 4
			offset += entryLen

			if pos+int(unsafe.Sizeof(attributeSet{})) > offset {
				continue
			}
			returned := *(*attributeSet)(unsafe.Pointer(&buf[pos]))
			pos += int(unsafe.Sizeof(attributeSet{}))

			if returned.commonattr&attrCmnError != 0 {
				errCode := *(*uint32)(unsafe.Pointer(&buf[pos]))
				pos += 4
				if errCode != 0 {
					continue
				}
			}

			var name string
			if returned.commonattr&attrCmnName != 0 {
				ref := *(*attrReference)(unsafe.Pointer(&buf[pos]))
				nameStart := pos + int(ref.dataOffset)
				pos += int(unsafe.Sizeof(attrReference{}))
				if nameStart >= 0 && nameStart < offset {
					maxLen := int(ref.length)
					if nameStart+maxLen > offset {
						maxLen = offset - nameStart
					}
					nameBytes := buf[nameStart : nameStart+maxLen]
					// Trim trailing NUL byte(s).
					for len(nameBytes) > 0 && nameBytes[len(nameBytes)-1] == 0 {
						nameBytes = nameBytes[:len(nameBytes)-1]
					}
					name = string(nameBytes)
				}
			}

			if name == "" || name == "." || name == ".." {
				continue
			}

			var objType uint32
			if returned.commonattr&attrCmnObjType != 0 {
				objType = *(*uint32)(unsafe.Pointer(&buf[pos]))
				pos += 4
			}

			var modSec, modNsec int64
			if returned.commonattr&attrCmnModTime != 0 {
				ts := *(*syscall.Timespec)(unsafe.Pointer(&buf[pos]))
				modSec, modNsec = ts.Sec, ts.Nsec
				pos += int(unsafe.Sizeof(syscall.Timespec{}))
			}

			var accSec, accNsec int64
			if returned.commonattr&attrCmnAccTime != 0 {
				ts := *(*syscall.Timespec)(unsafe.Pointer(&buf[pos]))
				accSec, accNsec = ts.Sec, ts.Nsec
				pos += int(unsafe.Sizeof(syscall.Timespec{}))
			}

			var flags uint32
			if returned.commonattr&attrCmnFlags != 0 {
				flags = *(*uint32)(unsafe.Pointer(&buf[pos]))
				pos += 4
			}

			var fileID uint64
			if returned.commonattr&attrCmnFileID != 0 {
				fileID = *(*uint64)(unsafe.Pointer(&buf[pos]))
				pos += 8
			}

			var hasEntryCount bool
			var entryCount uint32
			if returned.dirattr&attrDirEntryCount != 0 {
				entryCount = *(*uint32)(unsafe.Pointer(&buf[pos]))
				pos += 4
				hasEntryCount = true
			}

			if objType == vtypeDir {
				// Skip firmlinks (e.g., /System/Volumes/Data firmlinks) to avoid loops or duplicate traversal.
				if flags&sfFirmlink != 0 {
					continue
				}
				subPath := joinPath(dirPath, name)
				if exclude != nil && exclude.match(subPath) {
					continue
				}
				if hasEntryCount && entryCount == 0 {
					emptyDirs = append(emptyDirs, subPath)
				} else {
					subdirs = append(subdirs, subPath)
				}
				continue
			}

			if exclude != nil && exclude.match(joinPath(dirPath, name)) {
				continue
			}

			var linkCount uint32 = 1
			if returned.fileattr&attrFileLinkCount != 0 {
				linkCount = *(*uint32)(unsafe.Pointer(&buf[pos]))
				pos += 4
			}

			var totalSize int64
			if returned.fileattr&attrFileTotalSize != 0 {
				totalSize = *(*int64)(unsafe.Pointer(&buf[pos]))
				pos += 8
			}

			var allocSize int64
			if returned.fileattr&attrFileAllocSize != 0 {
				allocSize = *(*int64)(unsafe.Pointer(&buf[pos]))
				pos += 8
			}

			// Deduplicate hardlinks (files with linkCount > 1).
			if linkCount > 1 && fileID != 0 && seen != nil {
				if !seen.add(fileID) {
					continue
				}
			}

			var fileSize int64
			if apparentSize {
				fileSize = totalSize
			} else {
				fileSize = allocSize
			}
			ownSize += fileSize
			ownFiles++

			// Use the newer of atime and mtime (handles noatime/relatime mounts accurately).
			sec, nsec := accSec, accNsec
			if modSec > sec || (modSec == sec && modNsec > nsec) {
				sec, nsec = modSec, modNsec
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
					path:    joinPath(dirPath, name),
					size:    fileSize,
					modTime: ft,
				})
			}
		}
	}

	if maxSec > 0 || maxNsec > 0 {
		newestTime = time.Unix(maxSec, maxNsec)
	}
	return ownSize, ownFiles, newestTime, subdirs, emptyDirs, largeFiles, nil
}
