package uploadroot

import (
	"fmt"
	"math"
	"syscall"
)

// FreeSpace returns the bytes available to the service on the Upload Root's volume
// (what an unprivileged writer may still use, excluding root-reserved blocks).
func (r *Root) FreeSpace() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(r.real, &st); err != nil {
		return 0, fmt.Errorf("statfs Upload Root: %w", err)
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(free), nil
}
