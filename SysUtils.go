//go:build windows && amd64

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ----------------------------------------------------------------------------
// Win32-Konstanten
// ----------------------------------------------------------------------------

const (
	winCREATE_NO_WINDOW                   = 0x08000000
	winIDLE_PRIORITY_CLASS                = 0x00000040
	winENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
	winFO_DELETE                          = 0x0003
	winFOF_SILENT                         = 0x0004
	winFOF_NOCONFIRMATION                 = 0x0010
	winFOF_ALLOWUNDO                      = 0x0040
	winFOF_NOERRORUI                      = 0x0400
)

// ----------------------------------------------------------------------------
// Win32-Prozeduren
// ----------------------------------------------------------------------------

var (
	modShell32          = syscall.NewLazyDLL("shell32.dll")
	procSHFileOperation = modShell32.NewProc("SHFileOperationW")

	modKernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetConsMode           = modKernel32.NewProc("GetConsoleMode")
	procSetConsMode           = modKernel32.NewProc("SetConsoleMode")
	procOpenProcess           = modKernel32.NewProc("OpenProcess")
	procGetExitCode           = modKernel32.NewProc("GetExitCodeProcess")
	procCloseHandle           = modKernel32.NewProc("CloseHandle")
	procCreateFile            = modKernel32.NewProc("CreateFileW")
	procSetFileTime           = modKernel32.NewProc("SetFileTime")
	procSetConsoleCtrlHandler = modKernel32.NewProc("SetConsoleCtrlHandler")
	procGetLongPathNameW      = modKernel32.NewProc("GetLongPathNameW")
	procQueryFullProcessImage = modKernel32.NewProc("QueryFullProcessImageNameW")
)

// shellFileOpStruct entspricht SHFILEOPSTRUCTW im Win64-ABI (natürliches
// Alignment). Offsets: hwnd 0, wFunc 8 (+4 Pad), pFrom 16, pTo 24,
// fFlags 32 (+2 Pad), fAnyOperationsAborted 36, hNameMappings 40,
// lpszProgressTitle 48 — Gesamtgröße 56 Bytes. Offset 40 ist bereits
// 8-Byte-aligned, daher KEIN Padding nach fAnyOperationsAborted.
// fAnyOperationsAborted wird in sendToRecycleBin ausgewertet (FIX WIN-01).
type shellFileOpStruct struct {
	hwnd                  uintptr // HWND
	wFunc                 uint32  // UINT
	_pad1                 uint32  // Alignment: pFrom auf 8-Byte-Grenze
	pFrom                 uintptr // LPCWSTR — doppelt null-terminiert
	pTo                   uintptr // LPCWSTR
	fFlags                uint16  // FILEOP_FLAGS
	_pad2                 uint16  // Alignment: fAnyOperationsAborted auf 4-Byte-Grenze
	fAnyOperationsAborted int32   // BOOL — nach dem Call prüfen!
	hNameMappings         uintptr // LPVOID — immer 0
	lpszProgressTitle     uintptr // LPCWSTR — immer 0
}

// Compile-Time-Prüfung des ABI-Layouts: stimmt ein Offset oder die Größe
// nicht, bricht der Build ab (Array mit negativer Länge ist ein Fehler).
var (
	_ [unsafe.Offsetof(shellFileOpStruct{}.hNameMappings) - 40]byte
	_ [40 - unsafe.Offsetof(shellFileOpStruct{}.hNameMappings)]byte
	_ [unsafe.Sizeof(shellFileOpStruct{}) - 56]byte
	_ [56 - unsafe.Sizeof(shellFileOpStruct{})]byte
)

// ----------------------------------------------------------------------------
// ANSI-Konsole aktivieren
// ----------------------------------------------------------------------------

func enableAnsiConsole() {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	ret, _, _ := procGetConsMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if ret == 0 {
		return
	}
	procSetConsMode.Call(uintptr(h), uintptr(mode|winENABLE_VIRTUAL_TERMINAL_PROCESSING))
}

// ----------------------------------------------------------------------------
// SetConsoleCtrlHandler: Window-Close / Logoff / Shutdown abfangen
// ----------------------------------------------------------------------------

// setupConsoleCtrlHandler registriert einen Win32-Callback für CTRL_CLOSE_EVENT,
// CTRL_LOGOFF_EVENT und CTRL_SHUTDOWN_EVENT. Er ruft cancel() auf und gibt
// FFmpeg 3 Sekunden Zeit, die Ausgabedatei sauber zu finalisieren.
func setupConsoleCtrlHandler(cancel func()) {
	cb := syscall.NewCallback(func(ctrlType uint32) uintptr {
		const (
			CTRL_CLOSE_EVENT    = 2
			CTRL_LOGOFF_EVENT   = 5
			CTRL_SHUTDOWN_EVENT = 6
		)
		if ctrlType != CTRL_CLOSE_EVENT &&
			ctrlType != CTRL_LOGOFF_EVENT &&
			ctrlType != CTRL_SHUTDOWN_EVENT {
			return 0
		}
		cancel()
		if davinciMode || splitMode || joinMode {
			pAbort.Println("Window closed. Aborting...")
		} else {
			pAbort.Println("Window closed. Finishing current task cleanly (preview will be saved)...")
		}
		time.Sleep(3 * time.Second)
		return 1
	})
	procSetConsoleCtrlHandler.Call(cb, 1)
}

// ----------------------------------------------------------------------------
// sendToRecycleBin: FIX WIN-01 — fAnyOperationsAborted auswerten
// ----------------------------------------------------------------------------

// sendToRecycleBin verschiebt filePath in den Papierkorb. Auf UNC-/Netzwerk-
// pfaden (kein Papierkorb vorhanden) wird stattdessen os.Remove verwendet.
// Lokale Long-Path-Pfade (\\?\C:\...) werden normal gehandhabt.
func sendToRecycleBin(filePath string) error {
	upper := strings.ToUpper(filePath)
	isUNC := (strings.HasPrefix(filePath, `\\`) && !strings.HasPrefix(filePath, `\\?\`)) ||
		strings.HasPrefix(upper, `\\?\UNC\`)
	if isUNC {
		if err := os.Remove(filePath); err != nil {
			return fmt.Errorf("SysUtils.go: sendToRecycleBin (UNC remove): %w", err)
		}
		return nil
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}
	cleanPath := strings.TrimPrefix(absPath, `\\?\`)

	from, err := syscall.UTF16FromString(cleanPath)
	if err != nil {
		return fmt.Errorf("SysUtils.go: sendToRecycleBin (UTF16FromString): %w", err)
	}
	from = append(from, 0) // doppelte Null-Terminierung für SHFILEOPSTRUCTW

	op := shellFileOpStruct{
		wFunc:  winFO_DELETE,
		pFrom:  uintptr(unsafe.Pointer(&from[0])),
		fFlags: winFOF_NOCONFIRMATION | winFOF_SILENT | winFOF_ALLOWUNDO | winFOF_NOERRORUI,
	}
	ret, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(from)

	if ret != 0 {
		return fmt.Errorf("SysUtils.go: SHFileOperation failed (code %d, path may be too long or no recycle bin support)", ret)
	}
	// FIX WIN-01: ret==0 bedeutet nur "kein API-Fehler".
	// fAnyOperationsAborted==1 bedeutet stille Ablehnung.
	if op.fAnyOperationsAborted != 0 {
		return errors.New("SysUtils.go: SHFileOperation aborted (no recycle bin support on this drive?)")
	}
	return nil
}

// ----------------------------------------------------------------------------
// getLongPathName: FIX WIN-03 — UNC-Pfade korrekt behandeln
// ----------------------------------------------------------------------------

func getLongPathName(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}

	const prefix = `\\?\`
	const uncPrefix = `\\?\UNC\`

	queryPath := absPath
	isUNC := strings.HasPrefix(absPath, `\\`) && !strings.HasPrefix(absPath, `\\?\`)
	if isUNC {
		queryPath = uncPrefix + strings.TrimPrefix(absPath, `\\`)
	} else if !strings.HasPrefix(queryPath, prefix) {
		queryPath = prefix + queryPath
	}

	ptr, err := syscall.UTF16PtrFromString(queryPath)
	if err != nil {
		return path
	}

	n, _, _ := procGetLongPathNameW.Call(uintptr(unsafe.Pointer(ptr)), 0, 0)
	if n == 0 {
		return path
	}

	buf := make([]uint16, n)
	ret, _, _ := procGetLongPathNameW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&buf[0])),
		n,
	)
	if ret == 0 {
		return path
	}

	resultPath := syscall.UTF16ToString(buf)
	// FIX WIN-03: UNC-Präfix spiegelbildlich zurückbauen.
	if strings.HasPrefix(resultPath, uncPrefix) {
		return `\\` + strings.TrimPrefix(resultPath, uncPrefix)
	}
	return strings.TrimPrefix(resultPath, prefix)
}

// ----------------------------------------------------------------------------
// copyTimestamps: Erstellungs- & Änderungsdatum vom Original übernehmen
// ----------------------------------------------------------------------------

func copyTimestamps(src, dest string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("SysUtils.go: copyTimestamps (stat src): %w", err)
	}
	sysInfo, ok := srcInfo.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return nil
	}
	ctime := sysInfo.CreationTime
	atime := sysInfo.LastAccessTime
	mtime := sysInfo.LastWriteTime

	destPath := dest
	if len(destPath) > 240 && !strings.HasPrefix(destPath, `\\?\`) {
		if strings.HasPrefix(destPath, `\\`) {
			destPath = `\\?\UNC\` + strings.TrimPrefix(destPath, `\\`)
		} else {
			destPath = `\\?\` + destPath
		}
	}
	destPtr, err := syscall.UTF16PtrFromString(destPath)
	if err != nil {
		return fmt.Errorf("SysUtils.go: copyTimestamps (UTF16PtrFromString): %w", err)
	}

	const (
		FILE_WRITE_ATTRIBUTES     = 0x00000100
		FILE_SHARE_READ_WRITE_DEL = 0x00000007
		OPEN_EXISTING             = 3
		FILE_ATTRIBUTE_NORMAL     = 0x00000080
	)

	h, _, e := procCreateFile.Call(
		uintptr(unsafe.Pointer(destPtr)),
		FILE_WRITE_ATTRIBUTES,
		FILE_SHARE_READ_WRITE_DEL,
		0, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, 0,
	)
	if h == uintptr(syscall.InvalidHandle) {
		return fmt.Errorf("SysUtils.go: copyTimestamps (CreateFile): %w", e)
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()

	ret, _, e := procSetFileTime.Call(
		h,
		uintptr(unsafe.Pointer(&ctime)),
		uintptr(unsafe.Pointer(&atime)),
		uintptr(unsafe.Pointer(&mtime)),
	)
	if ret == 0 {
		return fmt.Errorf("SysUtils.go: copyTimestamps (SetFileTime): %w", e)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Lock-Verwaltung: Prozess-Status-Prüfung
// ----------------------------------------------------------------------------

func getProcessImagePath(handle uintptr) string {
	var buf [1024]uint16
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessImage.Call(
		handle, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:size])
}

func isLockOwnerAlive(info lockInfo) bool {
	if info.PID <= 0 {
		return false
	}
	const (
		PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
		STILL_ACTIVE                      = 259
		ERROR_ACCESS_DENIED               = 5
	)
	handle, _, callErr := procOpenProcess.Call(
		PROCESS_QUERY_LIMITED_INFORMATION, 0, uintptr(info.PID))
	if handle == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == ERROR_ACCESS_DENIED {
			return true
		}
		return false
	}
	defer func() { _, _, _ = procCloseHandle.Call(handle) }()

	var exitCode uint32
	ret, _, _ := procGetExitCode.Call(handle, uintptr(unsafe.Pointer(&exitCode)))
	if ret == 0 || exitCode != STILL_ACTIVE {
		return false
	}
	if info.OwnerImage != "" {
		currentImage := getProcessImagePath(handle)
		if currentImage != "" &&
			!strings.EqualFold(filepath.Base(currentImage), filepath.Base(info.OwnerImage)) {
			return false
		}
	}
	return true
}
