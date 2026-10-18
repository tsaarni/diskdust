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
	attrDirMountStatus = 0x00000004

	// File attributes (fileattr)
	attrFileLinkCount = 0x00000001
	attrFileTotalSize = 0x00000002
	attrFileAllocSize = 0x00000004

	// Options for getattrlistbulk
	fsoptNoFollow       = 0x00000001
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
	fileattr: attrFileLinkCount |
		attrFileTotalSize |
		attrFileAllocSize,
}

func scanDir(dirPath string, buf []byte, seen *inodeTracker, apparentSize bool) (ownSize, ownFiles int64, newestTime time.Time, subdirs []string, err error) {
	fd, err := syscall.Open(dirPath, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return 0, 0, time.Time{}, nil, err
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
			fsoptNoFollow,
			0,
		)
		if errno != 0 {
			return ownSize, ownFiles, newestTime, subdirs, errno
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

			if objType == vtypeDir {
				// Skip firmlinks (e.g., /System/Volumes/Data firmlinks) to avoid loops or duplicate traversal.
				if flags&sfFirmlink != 0 {
					continue
				}
				if dirPath == "/" {
					subdirs = append(subdirs, "/"+name)
				} else {
					subdirs = append(subdirs, dirPath+"/"+name)
				}
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

			if apparentSize {
				ownSize += totalSize
			} else {
				ownSize += allocSize
			}
			ownFiles++

			// Use the newer of atime and mtime (handles noatime/relatime mounts accurately).
			sec, nsec := accSec, accNsec
			if modSec > sec || (modSec == sec && modNsec > nsec) {
				sec, nsec = modSec, modNsec
			}
			if sec > maxSec || (sec == maxSec && nsec > maxNsec) {
				maxSec, maxNsec = sec, nsec
			}
		}
	}

	if maxSec > 0 || maxNsec > 0 {
		newestTime = time.Unix(maxSec, maxNsec)
	}
	return ownSize, ownFiles, newestTime, subdirs, nil
}
