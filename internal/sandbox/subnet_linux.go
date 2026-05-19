//go:build linux

package sandbox

import "sync"

// processAllocator is the singleton used by Wrap on Linux. Tests
// construct their own via newAllocator() to avoid coupling. Lives
// in a Linux-only file because nothing on non-Linux can reach Wrap.
var (
	processAllocatorOnce sync.Once
	processAllocator     *allocator
)

func defaultAllocator() *allocator {
	processAllocatorOnce.Do(func() {
		processAllocator = newAllocator()
	})
	return processAllocator
}
