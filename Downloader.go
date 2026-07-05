//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pterm/pterm"
)

// BtbN removed the floating "latest" release tag (the old fixed download URL
// now returns 404), so the newest autobuild has to be resolved at runtime via
// the GitHub API.
const ffmpegReleasesAPI = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases?per_page=5"

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// resolveFFmpegDownloadURL returns the download URL of the newest STABLE
// release-branch win64 GPL build (e.g. "ffmpeg-n8.1-latest-win64-gpl-8.1.zip").
// Master autobuilds are deliberately skipped: they track FFmpeg's development
// branch, which renames or removes encoder options without notice (2026-07 it
// dropped the -spatial_aq alias, which made the NVENC probe fail and look
// like a missing GPU). Fallback: any non-shared win64 GPL zip from the
// newest releases.
func resolveFFmpegDownloadURL(client *http.Client) (string, error) {
	req, err := http.NewRequest(http.MethodGet, ffmpegReleasesAPI, nil)
	if err != nil {
		return "", fmt.Errorf("Downloader.go: resolveFFmpegDownloadURL: %w", err)
	}
	// GitHub's API rejects requests without a User-Agent.
	req.Header.Set("User-Agent", "NVENCForge")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Downloader.go: resolveFFmpegDownloadURL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Downloader.go: resolveFFmpegDownloadURL: GitHub API returned HTTP %d", resp.StatusCode)
	}

	var releases []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&releases); err != nil {
		return "", fmt.Errorf("Downloader.go: resolveFFmpegDownloadURL (decode): %w", err)
	}

	var best, fallback string
	bestMajor, bestMinor := -1, -1
	for _, rel := range releases {
		for _, a := range rel.Assets {
			n := strings.ToLower(a.Name)
			if !strings.HasSuffix(n, ".zip") || !strings.Contains(n, "win64-gpl") ||
				strings.Contains(n, "shared") {
				continue
			}
			if fallback == "" {
				fallback = a.BrowserDownloadURL
			}
			// Stable release-branch build, e.g.
			// "ffmpeg-n8.1-latest-win64-gpl-8.1.zip" — take the highest version.
			var major, minor int
			if _, err := fmt.Sscanf(n, "ffmpeg-n%d.%d-latest-win64-gpl-", &major, &minor); err != nil {
				continue
			}
			if major > bestMajor || (major == bestMajor && minor > bestMinor) {
				bestMajor, bestMinor, best = major, minor, a.BrowserDownloadURL
			}
		}
	}
	if best != "" {
		return best, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("Downloader.go: resolveFFmpegDownloadURL: no win64-gpl zip found in the latest releases")
}

// The download spinner repaints its line in place, and plain conhost leaves
// remnants of longer predecessors standing ("Extracted: ffmpeg.exee (7s))") —
// same defect the Auto-CQ spinner had. Padding every phase text to the width
// of the longest one makes each repaint cover the whole previous line.
const dlSpinnerStartText = "Downloading FFmpeg (this may take a minute)..."

func dlSpinnerText(format string, args ...any) string {
	return fmt.Sprintf("%-*s", len(dlSpinnerStartText), fmt.Sprintf(format, args...))
}

// downloadFFmpeg downloads the newest stable release build (win64 GPL) from
// the official BtbN build repository and extracts only bin/ffmpeg.exe and
// bin/ffprobe.exe into targetDir. The ZIP is streamed to a temporary file
// rather than being loaded into RAM, keeping memory usage constant regardless
// of archive size.
func downloadFFmpeg(targetDir string) error {
	spinner, _ := pterm.DefaultSpinner.
		WithText(dlSpinnerStartText).
		Start()

	// ── Step 1: stream ZIP to a temp file ───────────────────────────────────
	tmp, err := os.CreateTemp("", "ffmpeg-download-*.zip")
	if err != nil {
		_ = spinner.Stop()
		return fmt.Errorf("Downloader.go: downloadFFmpeg (create temp): %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // always clean up, even on success

	// A bare http.Get uses DefaultClient with no timeout, so a stalled connection
	// would hang forever behind the spinner. Bound connect/handshake/response and
	// add a generous overall cap that still allows slow-but-progressing downloads.
	// The release is a rolling autobuild, so the binary cannot be hash-pinned;
	// integrity rests on HTTPS plus the post-extract "both executables present"
	// check below.
	client := &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	downloadURL, err := resolveFFmpegDownloadURL(client)
	if err != nil {
		_ = tmp.Close()
		_ = spinner.Stop()
		return err
	}

	resp, err := client.Get(downloadURL)
	if err != nil {
		_ = tmp.Close()
		_ = spinner.Stop()
		return fmt.Errorf("Downloader.go: downloadFFmpeg (http.Get): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_ = tmp.Close()
		_ = spinner.Stop()
		return fmt.Errorf("Downloader.go: downloadFFmpeg: server returned HTTP %d", resp.StatusCode)
	}

	written, err := io.Copy(tmp, resp.Body)
	if err != nil {
		_ = tmp.Close()
		_ = spinner.Stop()
		return fmt.Errorf("Downloader.go: downloadFFmpeg (stream to disk): %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = spinner.Stop()
		return fmt.Errorf("Downloader.go: downloadFFmpeg (close temp): %w", err)
	}

	spinner.UpdateText(dlSpinnerText("Download complete (%.1f MB). Extracting...", float64(written)/1048576))

	// ── Step 2: open ZIP and extract only the two target executables ─────────
	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		_ = spinner.Stop()
		return fmt.Errorf("Downloader.go: downloadFFmpeg (open zip): %w", err)
	}
	defer zr.Close()

	targets := map[string]string{
		"bin/ffmpeg.exe":  filepath.Join(targetDir, "ffmpeg.exe"),
		"bin/ffprobe.exe": filepath.Join(targetDir, "ffprobe.exe"),
	}
	extracted := 0

	for _, f := range zr.File {
		// The archive contains a top-level directory, e.g.:
		//   ffmpeg-n8.1-latest-win64-gpl-8.1/bin/ffmpeg.exe
		// We match by the trailing path component after the first directory.
		parts := strings.SplitN(filepath.ToSlash(f.Name), "/", 2)
		if len(parts) != 2 {
			continue
		}
		relPath := parts[1] // e.g. "bin/ffmpeg.exe"
		destPath, ok := targets[relPath]
		if !ok {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			_ = spinner.Stop()
			return fmt.Errorf("Downloader.go: downloadFFmpeg (open zip entry %q): %w", f.Name, err)
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			_ = rc.Close()
			_ = spinner.Stop()
			return fmt.Errorf("Downloader.go: downloadFFmpeg (mkdir): %w", err)
		}

		out, err := os.Create(destPath)
		if err != nil {
			_ = rc.Close()
			_ = spinner.Stop()
			return fmt.Errorf("Downloader.go: downloadFFmpeg (create %q): %w", filepath.Base(destPath), err)
		}

		if _, err := io.Copy(out, rc); err != nil {
			_ = out.Close()
			_ = rc.Close()
			_ = spinner.Stop()
			return fmt.Errorf("Downloader.go: downloadFFmpeg (extract %q): %w", filepath.Base(destPath), err)
		}
		_ = out.Close()
		_ = rc.Close()
		extracted++
		spinner.UpdateText(dlSpinnerText("Extracted: %s", filepath.Base(destPath)))
	}

	_ = spinner.Stop()

	if extracted == 0 {
		return fmt.Errorf("Downloader.go: downloadFFmpeg: neither ffmpeg.exe nor ffprobe.exe found in archive")
	}
	if extracted < 2 {
		return fmt.Errorf("Downloader.go: downloadFFmpeg: only %d of 2 executables found in archive", extracted)
	}

	pOK.Println("FFmpeg auto-download complete: ffmpeg.exe + ffprobe.exe installed.")
	return nil
}
