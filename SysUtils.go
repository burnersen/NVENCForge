//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

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

	// GetDriveTypeW-Rückgaben, die keinen Papierkorb besitzen.
	winDRIVE_REMOVABLE = 2
	winDRIVE_REMOTE    = 4
	winDRIVE_CDROM     = 5
)

// Papierkorb-Einstellungen liegen pro Volume unter HKEY_CURRENT_USER. Fehlt
// der Eintrag, gilt der Windows-Standard und wir treffen keine Annahme.
const (
	recycleBinVolumeKey  = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\BitBucket\Volume\`
	recycleBinBytesPerMB = 1024 * 1024
)

// ----------------------------------------------------------------------------
// Win32-Prozeduren
// ----------------------------------------------------------------------------

var (
	modShell32            = syscall.NewLazyDLL("shell32.dll")
	procSHFileOperation   = modShell32.NewProc("SHFileOperationW")
	procSHQueryRecycleBin = modShell32.NewProc("SHQueryRecycleBinW")

	modKernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetVolumePathName     = modKernel32.NewProc("GetVolumePathNameW")
	procGetVolumeNameForMntP  = modKernel32.NewProc("GetVolumeNameForVolumeMountPointW")
	procGetDriveType          = modKernel32.NewProc("GetDriveTypeW")
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

// shQueryRBInfo entspricht SHQUERYRBINFO im Win64-ABI: cbSize (4 Byte) +
// 4 Byte Padding + zwei int64 = 24 Byte. Damit fragen wir den Füllstand des
// Papierkorbs ab, um nach dem Löschen zu prüfen, ob die Datei dort ankam.
type shQueryRBInfo struct {
	cbSize      uint32
	_pad        uint32
	i64Size     int64
	i64NumItems int64
}

var (
	_ [unsafe.Sizeof(shQueryRBInfo{}) - 24]byte
	_ [24 - unsafe.Sizeof(shQueryRBInfo{})]byte
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
// Papierkorb: Vorprüfung — kann dieses Laufwerk die Datei aufnehmen?
// ----------------------------------------------------------------------------

// errRecycleNotVerified meldet den schlimmsten Fall: Windows hat die Datei
// gelöscht, sie ist aber nicht im Papierkorb angekommen (also endgültig weg).
// Der Aufrufer muss das deutlich sichtbar melden — nicht als "behalten".
var errRecycleNotVerified = errors.New("file was deleted but did not arrive in the recycle bin")

// recycleBinCheck ist das Ergebnis der Vorprüfung. volumeRoot ist der
// Einhängepunkt des Laufwerks ("C:\" oder "C:\Festplatte1\") und wird auch
// für die Füllstands-Abfrage nach dem Löschen gebraucht.
type recycleBinCheck struct {
	canRecycle bool
	reason     string // nur gefüllt, wenn canRecycle == false
	volumeRoot string
}

// isUNCPath erkennt Netzwerkpfade. \\?\C:\... ist trotz führender Backslashes
// ein lokaler Long-Path und damit KEIN UNC-Pfad.
func isUNCPath(filePath string) bool {
	upper := strings.ToUpper(filePath)
	return (strings.HasPrefix(filePath, `\\`) && !strings.HasPrefix(filePath, `\\?\`)) ||
		strings.HasPrefix(upper, `\\?\UNC\`)
}

// recycleSizeText schreibt Größen so, wie ein Mensch sie liest: bis knapp
// unter 1 GB in MB, darüber in GB.
func recycleSizeText(bytes int64) string {
	mb := float64(bytes) / recycleBinBytesPerMB
	if mb < 1024 {
		return fmt.Sprintf("%.0f MB", mb)
	}
	return fmt.Sprintf("%.1f GB", mb/1024)
}

// recycleBinFitsLimit sagt, ob eine Datei ins Papierkorb-Limit passt.
// sizeBytes < 0 heißt "Größe unbekannt" — dann urteilen wir nicht.
// capMB == 0 ist ein Papierkorb ohne Platz und nimmt gar nichts auf.
func recycleBinFitsLimit(sizeBytes int64, capMB uint32) bool {
	if sizeBytes < 0 {
		return true
	}
	if capMB == 0 {
		return false
	}
	return sizeBytes <= int64(capMB)*recycleBinBytesPerMB
}

// volumeRootOf liefert den Einhängepunkt, auf dem filePath liegt. Bei als
// Ordner eingehängten Platten ist das z. B. "C:\Festplatte1\" — genau dort
// liegt auch deren eigener Papierkorb.
func volumeRootOf(filePath string) (string, error) {
	pathUTF16, err := syscall.UTF16PtrFromString(filePath)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, syscall.MAX_PATH+1)
	ret, _, callErr := procGetVolumePathName.Call(
		uintptr(unsafe.Pointer(pathUTF16)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)))
	if ret == 0 {
		return "", fmt.Errorf("SysUtils.go: GetVolumePathNameW: %w", callErr)
	}
	return syscall.UTF16ToString(buf), nil
}

// volumeRegistryKeyOf übersetzt einen Einhängepunkt in den Registry-Unterkey
// seines Volumes, z. B. "...\BitBucket\Volume\{16022c82-...}".
func volumeRegistryKeyOf(volumeRoot string) (string, error) {
	rootUTF16, err := syscall.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 64) // "\\?\Volume{...}\" ist 49 Zeichen lang
	ret, _, callErr := procGetVolumeNameForMntP.Call(
		uintptr(unsafe.Pointer(rootUTF16)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)))
	if ret == 0 {
		return "", fmt.Errorf("SysUtils.go: GetVolumeNameForVolumeMountPointW: %w", callErr)
	}
	guid := syscall.UTF16ToString(buf) // \\?\Volume{GUID}\
	guid = strings.TrimSuffix(strings.TrimPrefix(guid, `\\?\`), `\`)
	guid = strings.TrimPrefix(guid, "Volume")
	return recycleBinVolumeKey + guid, nil
}

func driveTypeOf(volumeRoot string) uint32 {
	rootUTF16, err := syscall.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return 0
	}
	ret, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(rootUTF16)))
	return uint32(ret)
}

// regDWORDValue liest einen DWORD-Wert unter HKEY_CURRENT_USER. Fehlt er,
// ist ok == false — das ist kein Fehler, sondern "nicht konfiguriert".
func regDWORDValue(subKey, valueName string) (uint32, bool) {
	keyUTF16, err := syscall.UTF16PtrFromString(subKey)
	if err != nil {
		return 0, false
	}
	var handle syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, keyUTF16, 0,
		syscall.KEY_READ, &handle); err != nil {
		return 0, false
	}
	defer syscall.RegCloseKey(handle)

	nameUTF16, err := syscall.UTF16PtrFromString(valueName)
	if err != nil {
		return 0, false
	}
	var valueType, data uint32
	size := uint32(unsafe.Sizeof(data))
	if err := syscall.RegQueryValueEx(handle, nameUTF16, nil, &valueType,
		(*byte)(unsafe.Pointer(&data)), &size); err != nil {
		return 0, false
	}
	if valueType != syscall.REG_DWORD {
		return 0, false
	}
	return data, true
}

// checkRecycleBin klärt VOR dem Löschen, ob die Datei im Papierkorb landen
// kann. Hintergrund: Ist die Datei größer als das Papierkorb-Limit des
// Laufwerks, fragt Windows normalerweise "endgültig löschen?" — unser Flag
// FOF_NOCONFIRMATION beantwortet diese Rückfrage mit JA, und zwar völlig
// lautlos (ret==0, fAnyOperationsAborted==0). Dieser Fall ist deshalb nur
// vorher erkennbar, nicht am Rückgabewert.
//
// Wo Windows keine Auskunft gibt (kein Registry-Eintrag, Pfad nicht
// auflösbar), urteilen wir NICHT und lassen den normalen Weg zu — die
// Nachkontrolle in sendToRecycleBin fängt solche Fälle noch ab.
func checkRecycleBin(filePath string, sizeBytes int64) recycleBinCheck {
	if isUNCPath(filePath) {
		return recycleBinCheck{reason: "network path has no recycle bin"}
	}
	root, err := volumeRootOf(filePath)
	if err != nil {
		return recycleBinCheck{canRecycle: true}
	}
	switch driveTypeOf(root) {
	case winDRIVE_REMOTE:
		return recycleBinCheck{reason: "network drive " + root + " has no recycle bin", volumeRoot: root}
	case winDRIVE_REMOVABLE:
		return recycleBinCheck{reason: "removable drive " + root + " has no recycle bin", volumeRoot: root}
	case winDRIVE_CDROM:
		return recycleBinCheck{reason: "read-only drive " + root + " has no recycle bin", volumeRoot: root}
	}
	subKey, err := volumeRegistryKeyOf(root)
	if err != nil {
		return recycleBinCheck{canRecycle: true, volumeRoot: root}
	}
	if nuke, ok := regDWORDValue(subKey, "NukeOnDelete"); ok && nuke == 1 {
		return recycleBinCheck{
			reason:     "recycle bin is switched off for " + root,
			volumeRoot: root,
		}
	}
	capMB, ok := regDWORDValue(subKey, "MaxCapacity")
	if !ok {
		return recycleBinCheck{canRecycle: true, volumeRoot: root}
	}
	if !recycleBinFitsLimit(sizeBytes, capMB) {
		return recycleBinCheck{
			reason: fmt.Sprintf("file is %s, but the recycle bin of %s only holds %s",
				recycleSizeText(sizeBytes), root,
				recycleSizeText(int64(capMB)*recycleBinBytesPerMB)),
			volumeRoot: root,
		}
	}
	return recycleBinCheck{canRecycle: true, volumeRoot: root}
}

// ----------------------------------------------------------------------------
// Papierkorb: Füllstand abfragen (Nachkontrolle)
// ----------------------------------------------------------------------------

// queryRecycleBin liest Anzahl und Größe der Elemente im Papierkorb des
// Laufwerks, auf dem volumeRoot liegt. ok == false heißt "keine Auskunft".
func queryRecycleBin(volumeRoot string) (shQueryRBInfo, bool) {
	info := shQueryRBInfo{cbSize: uint32(unsafe.Sizeof(shQueryRBInfo{}))}
	if volumeRoot == "" {
		return info, false
	}
	rootUTF16, err := syscall.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return info, false
	}
	ret, _, _ := procSHQueryRecycleBin.Call(
		uintptr(unsafe.Pointer(rootUTF16)),
		uintptr(unsafe.Pointer(&info)))
	if ret != 0 { // Rückgabe ist ein HRESULT, S_OK == 0
		return info, false
	}
	return info, true
}

// recycleBinAccepted vergleicht den Füllstand vor und nach dem Löschen.
// Es genügt, dass Anzahl ODER Größe gewachsen ist: läuft der Papierkorb über,
// wirft Windows gleichzeitig ältere Elemente heraus — dann sinkt die Anzahl,
// die Größe wächst aber durch unsere große Datei trotzdem.
//
// Grenze dieser Messung: Leert oder füllt jemand den Papierkorb im selben
// Moment, kann das Urteil kippen. Der verlässliche Schutz ist deshalb die
// Vorprüfung; dies hier ist nur das Netz für Fälle, die sie nicht kennt.
func recycleBinAccepted(before, after shQueryRBInfo) bool {
	return after.i64NumItems > before.i64NumItems || after.i64Size > before.i64Size
}

// ----------------------------------------------------------------------------
// sendToRecycleBin: FIX WIN-01 — fAnyOperationsAborted auswerten
// ----------------------------------------------------------------------------

// sendToRecycleBin verschiebt filePath in den Papierkorb — aber nur, wenn der
// Papierkorb die Datei auch aufnehmen kann. Kann er es nicht (Netzwerkpfad,
// Wechselmedium, abgeschalteter Papierkorb, Datei größer als das Limit), wird
// NICHT gelöscht: der Aufrufer soll das Original dann stehen lassen. Nach dem
// Löschen wird zusätzlich geprüft, ob die Datei wirklich im Papierkorb liegt.
// Lokale Long-Path-Pfade (\\?\C:\...) werden normal gehandhabt.
func sendToRecycleBin(filePath string) error {
	sizeBytes := int64(-1) // -1 = Größe unbekannt, dann keine Limit-Aussage
	if info, err := os.Stat(filePath); err == nil {
		sizeBytes = info.Size()
	}
	check := checkRecycleBin(filePath, sizeBytes)
	if !check.canRecycle {
		return errors.New(check.reason)
	}
	before, haveBefore := queryRecycleBin(check.volumeRoot)

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
	// Nachkontrolle: Die Datei ist weg — liegt sie auch wirklich im Papierkorb?
	if haveBefore {
		if after, ok := queryRecycleBin(check.volumeRoot); ok && !recycleBinAccepted(before, after) {
			return errRecycleNotVerified
		}
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
