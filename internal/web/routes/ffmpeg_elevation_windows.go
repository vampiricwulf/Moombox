package routes

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// encodePowerShellCommand turns a PowerShell script source string into the
// base64-of-UTF16LE form that powershell.exe's `-EncodedCommand` parameter
// expects. The TOCTOU benefit of this format is that the script never has
// to be written to disk between Moombox preparing it and the elevated
// PowerShell parsing it. Audit reports/web.md S-10.
func encodePowerShellCommand(script string) (string, error) {
	utf16 := utf16Encode(script)
	if len(utf16) == 0 && script != "" {
		return "", fmt.Errorf("utf-16 encode produced no output")
	}
	buf := make([]byte, len(utf16)*2)
	for i, r := range utf16 {
		binary.LittleEndian.PutUint16(buf[i*2:], r)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// utf16Encode is a minimal UTF-16 encoder that handles BMP runes inline and
// supplementary-plane runes via surrogate pairs. Anything outside valid
// Unicode (lone surrogates, runes above 0x10FFFF) is coerced to U+FFFD.
func utf16Encode(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0 || r > 0x10FFFF || (r >= 0xD800 && r <= 0xDFFF):
			out = append(out, 0xFFFD)
		case r <= 0xFFFF:
			out = append(out, uint16(r))
		default:
			r -= 0x10000
			out = append(out, 0xD800|uint16(r>>10), 0xDC00|uint16(r&0x3FF))
		}
	}
	return out
}

var (
	advapi32          = syscall.NewLazyDLL("advapi32.dll")
	openProcessToken  = advapi32.NewProc("OpenProcessToken")
	getTokenInfo      = advapi32.NewProc("GetTokenInformation")

	shell32           = syscall.NewLazyDLL("shell32.dll")
	shellExecuteExW   = shell32.NewProc("ShellExecuteExW")

	kernel32Elev      = syscall.NewLazyDLL("kernel32.dll")
	waitForSingleObj  = kernel32Elev.NewProc("WaitForSingleObject")
)

// shellExecuteInfo mirrors the Windows SHELLEXECUTEINFOW structure.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       uintptr
	lpFile       uintptr
	lpParameters uintptr
	lpDirectory  uintptr
	nShow        int32
	_pad0        int32 // alignment padding on 64-bit
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      uintptr
	hkeyClass    uintptr
	dwHotKey     uint32
	_pad1        uint32 // alignment padding on 64-bit
	hIcon        uintptr
	hProcess     uintptr
}

const (
	seeMaskNoCloseProcess = 0x00000040
	swHide               = 0
	tokenQuery           = 0x0008
	tokenElevation       = 20 // TokenElevation information class
)

// isElevated returns true if the current process is running with administrator
// privileges (elevated via UAC).
func isElevated() bool {
	var token syscall.Token
	handle, _ := syscall.GetCurrentProcess()

	ret, _, _ := openProcessToken.Call(
		uintptr(handle),
		uintptr(tokenQuery),
		uintptr(unsafe.Pointer(&token)),
	)
	if ret == 0 {
		return false
	}
	defer token.Close()

	var elevation uint32
	var returnLength uint32
	ret, _, _ = getTokenInfo.Call(
		uintptr(token),
		uintptr(tokenElevation),
		uintptr(unsafe.Pointer(&elevation)),
		4,
		uintptr(unsafe.Pointer(&returnLength)),
	)
	if ret == 0 {
		return false
	}
	return elevation != 0
}

// runElevated launches a PowerShell script with UAC elevation ("Run as
// Administrator") using ShellExecuteExW with the "runas" verb. Returns the
// process handle for waiting, or an error if the UAC prompt was declined.
//
// Audit reports/web.md S-10 — switched from `-File <path>` to
// `-EncodedCommand <base64-utf16le>` so the script never touches the
// filesystem. Removes the TOCTOU window where a local attacker could swap
// the temp file between WriteFile and ShellExecuteEx.
func runElevated(script string) (syscall.Handle, error) {
	verb, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return 0, fmt.Errorf("utf16 verb: %w", err)
	}
	file, err := syscall.UTF16PtrFromString("powershell.exe")
	if err != nil {
		return 0, fmt.Errorf("utf16 file: %w", err)
	}
	encoded, err := encodePowerShellCommand(script)
	if err != nil {
		return 0, fmt.Errorf("encode powershell command: %w", err)
	}
	params, err := syscall.UTF16PtrFromString(
		`-NoProfile -ExecutionPolicy Bypass -EncodedCommand ` + encoded,
	)
	if err != nil {
		return 0, fmt.Errorf("utf16 params: %w", err)
	}

	sei := shellExecuteInfo{
		cbSize:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       uintptr(unsafe.Pointer(verb)),
		lpFile:       uintptr(unsafe.Pointer(file)),
		lpParameters: uintptr(unsafe.Pointer(params)),
		nShow:        swHide,
	}

	ret, _, callErr := shellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	if ret == 0 {
		return 0, fmt.Errorf("ShellExecuteEx failed (UAC declined?): %w", callErr)
	}
	return syscall.Handle(sei.hProcess), nil
}

// waitForProcess waits for a process handle to exit within the given timeout.
// Closes the handle when done. Returns nil on success, or an error on timeout or failure.
func waitForProcess(handle syscall.Handle, timeout time.Duration) error {
	defer syscall.CloseHandle(handle)
	millis := uint32(timeout.Milliseconds())
	ret, _, err := waitForSingleObj.Call(
		uintptr(handle),
		uintptr(millis),
	)
	switch ret {
	case 0: // WAIT_OBJECT_0 — process exited
		return nil
	case 0x00000102: // WAIT_TIMEOUT
		return fmt.Errorf("process did not finish within %v", timeout)
	default: // WAIT_FAILED or other
		return fmt.Errorf("WaitForSingleObject failed: %w", err)
	}
}
