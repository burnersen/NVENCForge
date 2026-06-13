//go:build windows && amd64

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const helpFileName = "NVENCForge_Help.txt"

// helpFileContent is the user-facing manual written next to the executable. It
// deliberately omits any developer-only switches.
const helpFileContent = `============================================================
  NVENCForge - Help
  H.265 NVENC batch encoder + MKV/MP4 stream toolkit
============================================================

WHAT IT DOES
  NVENCForge converts video files to H.265 (HEVC) using your
  Nvidia GPU (NVENC), and includes a toolkit to split, extract
  and merge audio / subtitle / video streams. Subtitles are
  cleaned automatically.

QUICK START
  Drag one or more video files (or a folder) onto NVENCForge.exe.
  With no arguments it processes every supported video in the
  current folder. Converted files are written to an "output"
  subfolder; after a successful conversion the original is moved
  to the recycle bin (restorable from there).

CONVERSION OPTIONS
  -NNNN          Set the maximum target bitrate in kbit/s.
                 Example:  NVENCForge.exe -10000 video.mp4
  -original      Keep the original resolution (no downscaling);
                 the bitrate cap is raised automatically. (alias: -orig)
  -copyaudio     Copy all audio tracks 1:1 (no AAC re-encode).
                 Use this for plain viewing when you want the
                 original sound untouched. (alias: -ca)
  -av1           Encode AV1 instead of H.265 (needs an RTX 40
                 series GPU or newer). Same quality at roughly
                 25-30% smaller files; output is ".av1.mkv".
                 Note: current Apple TV models have no AV1
                 hardware decoding - H.265 stays the default.
  -keep          Keep the original files: after a successful
                 conversion they are NOT moved to the recycle bin.
                 The output lives in its own folder, so nothing is
                 overwritten. Use this if you want both files.
  -shutdown      Shut the PC down 30 s after the batch finishes
                 ("shutdown /a" cancels it).
  Options can be combined, e.g.:  -original -copyaudio -shutdown
  Always list options FIRST, then the files to process.
  -streams must be the very first argument.

  Without -original, videos above 1080p are downscaled and lightly
  sharpened. By default, audio in formats unsuitable for editing
  is re-encoded to AAC (DaVinci-friendly) and compatible audio is
  copied unchanged; -copyaudio keeps every track exactly as-is.

STREAM TOOLKIT  (-streams)
  NVENCForge.exe -streams <files>
    - Drop a single MKV  -> split into MP4 + separate audio and
      subtitle files.
    - Drop an MP4/MOV    -> extract its audio and subtitle tracks.
    - Drop ONE video plus one or more audio / .srt files
      -> merge them into a new ".sub.mkv".
    - Run "NVENCForge.exe -streams" with NO files inside a folder
      -> batch mode: every MKV in that folder is split
      automatically (all tracks, no stereo mixes, no questions).
      You may start the same command a second time in parallel:
      each file is locked while it is processed, so the instances
      share the work without disturbing each other.
  When a file contains two or more selectable entries, you are
  asked which ones to extract (press Enter for all, or type the
  numbers, e.g. "1,3"). With a single track there is no question.
  Multichannel audio offers an extra "stereo mix" entry with its
  own number (saved as ".stereo.m4a"). It is only created when
  you select that number - Enter (= all) does NOT include it.
  Merging uses ONLY the files you drop: the base video contributes
  its picture only - audio or subtitles inside the base video are
  never carried over (a notice is shown if any get dropped).

SUBTITLE CLEANER
  Every extracted .srt is cleaned automatically: HTML/styling
  tags, invisible characters and advertising lines are removed.

FFMPEG
  ffmpeg.exe and ffprobe.exe are used for all processing. If they
  are missing, NVENCForge downloads them automatically. You may
  also place them next to NVENCForge.exe.

CONFIGURATION
  Encoder defaults live in "NVENCForge_Config.ini" next to the
  exe (created on first run). Edit it to change CQ, presets,
  resolution cap, audio bitrate, etc. Invalid values are reported
  at startup and fall back to their defaults individually; your
  other settings stay untouched.

OUTPUT & REQUIREMENTS
  Output folder:  output (next to the processed files)
  System:         Windows 10/11 x64, Nvidia GPU (Maxwell+)
                  with up-to-date drivers.
                  (The -streams toolkit needs no Nvidia GPU.)

  Press Ctrl+C during a conversion to stop; the partial result is
  saved as a playable ".preview.mkv" instead of being discarded.
`

// writeHelpFileIfMissing creates the help file next to the executable when it
// is absent. An existing file is never overwritten. Non-fatal on error: the
// tool runs regardless of whether the file could be written.
func writeHelpFileIfMissing() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("Help.go: writeHelpFileIfMissing: %w", err)
	}
	path := filepath.Join(filepath.Dir(exePath), helpFileName)
	if _, statErr := os.Stat(path); statErr == nil {
		return nil
	}
	if err := os.WriteFile(path, []byte(helpFileContent), 0o644); err != nil {
		return fmt.Errorf("Help.go: writeHelpFileIfMissing: %w", err)
	}
	return nil
}
