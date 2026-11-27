package main

import (
	"os"
	"syscall"
)

// openSourceShared opens the cookie source read-only and, unlike os.Open,
// with FILE_SHARE_DELETE: the running Moombox renews that file by renaming a
// temp file over it (writeFileAtomic), and os.Open's share mode would make
// that rename fail for as long as this read holds the file open.
func openSourceShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
