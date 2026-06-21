//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// embeddedSourceZip holds the build sources packed by build.bat at compile time.
// It is unpacked into a "sourcecode" folder so the executable can rebuild itself.
//
//go:embed embedded_source.zip
var embeddedSourceZip []byte

// extractEmbeddedSource unpacks the embedded build sources into a "sourcecode"
// folder next to the executable, but only when that folder does not yet exist,
// so existing edits are never overwritten. Non-fatal on error: the tool runs
// regardless of whether extraction succeeds.
func extractEmbeddedSource() error {
	if len(embeddedSourceZip) == 0 {
		return nil
	}
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("Embed.go: extractEmbeddedSource: %w", err)
	}
	destRoot := filepath.Join(filepath.Dir(exePath), "sourcecode")
	if _, statErr := os.Stat(destRoot); statErr == nil {
		return nil // already present, leave user edits untouched
	}

	zr, err := zip.NewReader(bytes.NewReader(embeddedSourceZip), int64(len(embeddedSourceZip)))
	if err != nil {
		return fmt.Errorf("Embed.go: extractEmbeddedSource: %w", err)
	}
	cleanRoot := filepath.Clean(destRoot)
	for _, f := range zr.File {
		target := filepath.Join(destRoot, filepath.FromSlash(f.Name))
		// Zip-slip guard: reject entries that would escape destRoot.
		if target != cleanRoot && !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		if err := extractZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

// extractZipFile writes a single zip entry to target, creating parent dirs.
func extractZipFile(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("Embed.go: extractZipFile: %w", err)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("Embed.go: extractZipFile: %w", err)
	}
	defer rc.Close()

	out, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("Embed.go: extractZipFile: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("Embed.go: extractZipFile: %w", err)
	}
	return nil
}
