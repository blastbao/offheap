package offheap

import (
	"fmt"
	"os"
	"syscall"

	"github.com/glycerine/gommap"
)

// The MmapMalloc struct represents either an anonymous, private
// region of memory (if path was "", or a memory mapped file if
// path was supplied to Malloc() at creation.
//
// Malloc() creates and returns an MmapMalloc struct, which can then
// be later Free()-ed. Malloc() calls request memory directly
// from the kernel via mmap(). Memory can optionally be backed
// by a file for simplicity/efficiency of saving to disk.
//
// For use when the Go GC overhead is too large, and you need to move
// the hash table off-heap.
//
type MmapMalloc struct {
	Path         string
	File         *os.File
	Fd           int
	FileBytesLen int64
	BytesAlloc   int64
	MMap         gommap.MMap // equiv to Mem, just avoids casts everywhere.
	Mem          []byte      // equiv to Mmap
}

// TruncateTo enlarges or shortens the file backing the
// memory map to be size newSize bytes. It only impacts
// the file underlying the mapping, not
// the mapping itself at this point.
func (mm *MmapMalloc) TruncateTo(newSize int64) {
	if mm.File == nil {
		panic("cannot call TruncateTo() on a non-file backed MmapMalloc.")
	}
	err := syscall.Ftruncate(int(mm.File.Fd()), newSize)
	if err != nil {
		panic(err)
	}
}

// Free releases the memory allocation back to the OS by removing
// the (possibly anonymous and private) memroy mapped file that
// was backing it. Warning: any pointers still remaining will crash
// the program if dereferenced.
func (mm *MmapMalloc) Free() {
	if mm.File != nil {
		mm.File.Close()
	}
	err := mm.MMap.UnsafeUnmap()
	if err != nil {
		panic(err)
	}
}

// Malloc creates a new memory region that is provided directly
// by OS via the mmap() call, and is thus not scanned by the Go
// garbage collector.
//
// If path is not empty then we memory map to the given path.
// Otherwise it is just like a call to malloc(): an anonymous memory allocation,
// outside the realm of the Go Garbage Collector.
// If numBytes is -1, then we take the size from the path file's size. Otherwise
// the file is expanded or truncated to be numBytes in size. If numBytes is -1
// then a path must be provided; otherwise we have no way of knowing the size
// to allocate, and the function will panic.
//
// The returned value's .Mem member holds a []byte pointing to the returned memory (as does .MMap, for use in other gommap calls).
func Malloc(numBytes int64, path string) *MmapMalloc {

	mm := MmapMalloc{
		Path: path,
	}

	flags := syscall.MAP_SHARED
	if path == "" {
		flags = syscall.MAP_ANON | syscall.MAP_PRIVATE
		mm.Fd = -1
		if numBytes < 0 {
			panic("numBytes was negative but path was also empty: don't know how much to allocate!")
		}
	} else {
		// 不能是目录
		if dirExists(mm.Path) {
			panic(fmt.Sprintf("path '%s' already exists as a directory, so cannot be used as a memory mapped file.", mm.Path))
		}
		// 不存在则创建文件，存在则打开
		if !fileExists(mm.Path) {
			file, err := os.Create(mm.Path)
			if err != nil {
				panic(err)
			}
			mm.File = file
		} else {
			file, err := os.OpenFile(mm.Path, os.O_RDWR, 0777)
			if err != nil {
				panic(err)
			}
			mm.File = file
		}
		// 提取 FD
		mm.Fd = int(mm.File.Fd())
	}

	sz := numBytes
	if path != "" {
		// file-backed memory
		if numBytes < 0 {
			var stat syscall.Stat_t
			if err := syscall.Fstat(mm.Fd, &stat); err != nil {
				panic(err)
			}
			sz = stat.Size
		} else {
			// set to the size requested
			err := syscall.Ftruncate(mm.Fd, numBytes)
			if err != nil {
				panic(err)
			}
		}
		mm.FileBytesLen = sz
	}

	// INVAR: sz holds non-negative length of memory/file.

	mm.BytesAlloc = sz
	prot := syscall.PROT_READ | syscall.PROT_WRITE

	vprintf("\n ------->> path = '%v',  mm.Fd = %v, with flags = %x, sz = %v,  prot = '%v'\n", path, mm.Fd, flags, sz, prot)

	// 调用 mmap 创建堆外内存，获取 []byte
	var mmap []byte
	var err error
	if mm.Fd == -1 {
		flags = syscall.MAP_ANON | syscall.MAP_PRIVATE
		mmap, err = syscall.Mmap(-1, 0, int(sz), prot, flags)
	} else {
		flags = syscall.MAP_SHARED
		mmap, err = syscall.Mmap(mm.Fd, 0, int(sz), prot, flags)
	}
	if err != nil {
		panic(err)
	}

	// duplicate member to avoid casts all over the place.
	mm.MMap = mmap
	mm.Mem = mmap

	return &mm
}

// BlockUntilSync() returns only once the file is synced to disk.
func (mm *MmapMalloc) BlockUntilSync() {
	mm.MMap.Sync(gommap.MS_SYNC)
}

// BackgroundSync schedules a sync to disk, but may return before it is done.
// Without a call to either BackgroundSync() or BlockUntilSync(), there
// is no guarantee that file has ever been written to disk at any point before
// the munmap() call that happens during Free(). See the man pages msync(2)
// and mmap(2) for details.
func (mm *MmapMalloc) BackgroundSync() {
	mm.MMap.Sync(gommap.MS_ASYNC)
}

// Growmap grows a memory mapping
func Growmap(oldmap *MmapMalloc, newSize int64) (newmap *MmapMalloc, err error) {

	if oldmap.Path == "" || oldmap.Fd <= 0 {
		return nil, fmt.Errorf("oldmap must be mapping to actual file, so we can grow it")
	}
	if newSize <= oldmap.BytesAlloc || newSize <= oldmap.FileBytesLen {
		return nil, fmt.Errorf("mapping in Growmap must be larger than before")
	}

	newmap = &MmapMalloc{}
	newmap.Path = oldmap.Path
	newmap.Fd = oldmap.Fd

	// set to the size requested
	err = syscall.Ftruncate(newmap.Fd, newSize)
	if err != nil {
		panic(err)
		return nil, fmt.Errorf("syscall.Ftruncate to grow %v -> %v returned error: '%v'", oldmap.BytesAlloc, newSize, err)
	}
	newmap.FileBytesLen = newSize
	newmap.BytesAlloc = newSize

	prot := syscall.PROT_READ | syscall.PROT_WRITE
	flags := syscall.MAP_SHARED

	p("\n ------->> path = '%v',  newmap.Fd = %v, with flags = %x, sz = %v,  prot = '%v'\n", newmap.Path, newmap.Fd, flags, newSize, prot)

	mmap, err := syscall.Mmap(newmap.Fd, 0, int(newSize), prot, flags)
	if err != nil {
		panic(err)
		return nil, fmt.Errorf("syscall.Mmap returned error: '%v'", err)
	}

	// duplicate member to avoid casts all over the place.
	newmap.MMap = mmap
	newmap.Mem = mmap

	return newmap, nil
}
