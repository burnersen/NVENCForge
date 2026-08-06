//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pterm/pterm"
)

const helpFileName = "NVENCForge_Help.txt"

// helpFileContent is the user-facing manual written next to the executable. It
// deliberately omits any developer-only switches.
const helpFileContent = `============================================================
  NVENCForge - Help
  H.265 NVENC batch encoder + DaVinci Resolve workflow & lossless tools
============================================================

------------------------------------------------------------
  THE SHORT VERSION  -  this is all most people ever need
------------------------------------------------------------

  Drag video files or a whole folder onto NVENCForge.exe.
  That is it. There is nothing to set up and nothing to type.

  What happens then:
    - Every video is converted to H.265, which usually cuts the
      file size by half or more at the same visible quality.
    - The right quality setting is MEASURED for each file
      individually, so you neither waste space nor lose quality.
    - Results land in an "output" subfolder. Your original is
      moved into an "originals" subfolder next to it and is
      never deleted - it waits there until you delete it.
    - Videos larger than 1080p are scaled down to 1080p and
      lightly sharpened. Use -original to keep the resolution.
    - Files that are already compressed efficiently are only
      repackaged, never re-encoded (that would just make them
      bigger). Files you converted before are skipped, so
      running NVENCForge a second time is always safe.

  Nothing to install: FFmpeg does the actual encoding work, and
  NVENCForge fetches a tested copy of it by itself on the first
  run - once, roughly 80 MB, no setup on your part.

  The three options people actually use:
    -original     keep the resolution (no downscale to 1080p)
    -copyaudio    leave the audio exactly as it is
    -av1          AV1 instead of H.265: smaller files, but it
                  needs an RTX 40 series card or newer

  Worth doing once - the "Send to" menu:
    Put NVENCForge into the Windows right-click menu and you can
    convert any video without opening a folder first.
      1. Press Win+R, type  shell:sendto  and press Enter.
      2. Drop a SHORTCUT to NVENCForge.exe into the folder that
         opens (right-drag the exe there and choose "Create
         shortcut here" - do not move the exe itself).
    From then on: right-click any video -> Send to -> NVENCForge.
    You can add a second shortcut with an option in its Target
    field (e.g. ...\NVENCForge.exe -original) to have both.

  For the complete option list, run:  NVENCForge.exe -help
  Everything below is detail you only need when you want it.

============================================================
  EVERYTHING ELSE
============================================================

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
  -mp4           Write a ".mp4" instead of ".mkv" - the container
                 nearly every device can open: iPhone/iPad (imports
                 into the Photos app), smart TVs, tablets, browsers.
                 The H.265 video is re-tagged "hvc1" (Apple and
                 DaVinci refuse the "hev1" tag FFmpeg sets by
                 default), audio becomes AAC where needed and
                 "+faststart" is added so playback starts at once.
                 A fresh source is encoded as usual and then
                 repackaged; a file you ALREADY converted
                 (".h265.mkv") is only repackaged - no second
                 encode - and the original .mkv is kept.
                 With more than one audio/subtitle track you are
                 asked which ones to keep (no answer within 30 s
                 keeps them all). Text subtitles go in as
                 "mov_text"; picture subtitles from Blu-rays cannot
                 be stored in MP4 at all and are reported.
                 AV1 plays on neither iPhones nor most TVs, so -mp4
                 always uses H.265 (an existing ".av1.mkv" is
                 skipped with a hint - re-run -mp4 on the original
                 source). (alias: -apple, the old name)
  -8bit          Encode in 8 bit instead of 10 bit. Only needed for
                 devices that cannot decode the "Main 10" profile -
                 some older TVs, beamers and Android phones show a
                 black screen or refuse the file. The picture may
                 show slight banding in dark gradients, which is
                 exactly what 10 bit avoids, so use this only when
                 a device actually refuses to play. Works with
                 every mode (H.265, AV1, -cpu, -mp4). Repackaging
                 an already converted file cannot change its bit
                 depth - that needs a real conversion.
  -cpu           Encode on the processor instead of the graphics
                 card - NO Nvidia card required (libx265, or
                 SVT-AV1 when combined with -av1). Everything else
                 is identical: downscaling, sharpening, audio,
                 bitrate caps, Auto-CQ and the file names.
                 It is much slower - roughly 40 minutes per hour of
                 1080p video on a modern 8-core CPU, clearly more
                 on older machines. Speed, quality and how many
                 cores may be used are set with the "cpu..." keys
                 in the config file; "encoder=cpu" there makes CPU
                 mode permanent. If no Nvidia card is found at
                 startup, NVENCForge offers CPU mode by itself
                 instead of refusing to run.
                 Tip: on the CPU, AV1 is both faster AND smaller at
                 equal quality - if your player handles AV1,
                 "-cpu -av1" is the better deal.
  -autocq        Find the best quality setting for every file
                 automatically. This is ON by default - you never
                 have to type it.
                 How it works: a few short sample scenes (the
                 hardest one is always included) are encoded at two
                 test settings and compared against the original
                 with VMAF, a measurement of how much visible
                 quality is left. The setting that should reach the
                 quality target is then verified by one more real
                 measurement before the actual encode starts. It
                 costs a minute or two per file and replaces all
                 guesswork about "which CQ should I use".
                 The target is "autoCQTargetVMAF" in the config
                 file (default 96 of 100 - at that level the
                 result is indistinguishable in normal viewing).
                 Sources that were already heavily compressed
                 cannot reach the target at all. NVENCForge then
                 measures how far they CAN go and picks the most
                 economical setting instead of wasting space on a
                 target that is out of reach. Two config keys
                 steer how thrifty it may be: "autoCQTolerance"
                 and "autoCQPlateauTolerance".
                 Works for H.265 and AV1 alike. Videos shorter
                 than 30 seconds skip the analysis. Turn it off
                 with -noautocq, or autoCQ=false in the config.
  -noautocq      Disable Auto-CQ for this run (overrides the
                 autoCQ=true config default).
  -cq NN         Force a fixed CQ for this run only: skips Auto-CQ
                 and ignores the configured CQ. The scale depends
                 on the codec (H.265 1-51; AV1 1-63 with -av1).
                 Example:  NVENCForge.exe -cq 28 video.mp4
  -keep          Keep the original files where they are: after a
                 successful conversion they are NOT moved at all.
                 The output lives in its own folder, so nothing is
                 overwritten. Use this if you want both files.
  -shutdown      Shut the PC down 30 s after the batch finishes
                 ("shutdown /a" cancels it).
  -help          Print the complete option list in the console and
                 exit. Also works as -h, -?, /? or --help, and can
                 stand anywhere in the command. Nothing is
                 downloaded and no graphics card is checked - you
                 get the list and nothing else.
  Options can be combined, e.g.:  -original -copyaudio -shutdown
  Always list options FIRST, then the files to process.
  -davinci, -split and -join must be the very first argument.

  Without -original, videos above 1080p are downscaled and lightly
  sharpened. By default, audio in formats unsuitable for editing
  is re-encoded to AAC (DaVinci-friendly) and compatible audio is
  copied unchanged; -copyaudio keeps every track exactly as-is.

FOR DAVINCI RESOLVE WORKFLOW  (-davinci)   re-encodes where needed
  NVENCForge.exe -davinci <files>
    - Drop a single MKV  -> split into a silent ".NoSound.mp4"
      plus separate audio and subtitle files (audio is re-encoded
      to AAC where DaVinci needs it, subtitles are converted to
      .srt and cleaned).
    - Drop an MP4/MOV    -> extract its audio and subtitle tracks
      and write a silent ".NoSound.mp4".
    - Drop ONE video plus one or more audio / .srt files
      -> merge them into a new ".sub.mkv".
    - Run "NVENCForge.exe -davinci" with NO files inside a folder
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
  Merging: dropped audio files REPLACE the audio of the base
  video. If you drop only subtitles, the audio already inside the
  base video is kept. Subtitles inside the base video are never
  carried over (a notice is shown if any get dropped).

LOSSLESS SPLIT / JOIN  (-split / -join)   1:1 copy, no re-encode
  NVENCForge.exe -split <files / folders>
    - Splits a video into its raw parts WITHOUT any re-encode and
      WITHOUT cleaning: a silent ".NoSound" picture (kept in the
      source container, mp4 stays mp4, everything else becomes mkv),
      every audio track in its native container (.ac3/.dts/.eac3/
      .m4a/.flac/.thd/.mka ...) and every subtitle untouched
      (.srt/.ass/.sup/.idx ...).
    - A single dropped file asks which tracks to extract (Enter =
      all). A dropped folder, or no files at all, runs batch mode:
      every supported video in the folder, all tracks, no questions,
      parallel-safe (same locking as -davinci).
    - Unlike -davinci the stereo-mix option is hidden here, because
      a downmix would be a re-encode.
  NVENCForge.exe -join <one .NoSound video> <audio/subtitle files>
    - Recombines the picture with the dropped audio and subtitle
      files into one ".joined.mkv", copying everything 1:1.
    - Dropped audio files REPLACE the audio of the base video; if
      you drop only subtitles, the base video keeps its own sound.
      Subtitles inside the base video are never carried over.
  Use -split / -join for a true lossless round-trip. Use -davinci
  when you need DaVinci-ready output (AAC audio, cleaned subtitles).

SUBTITLE CLEANER
  Every extracted .srt is cleaned automatically: HTML/styling
  tags, invisible characters and advertising lines are removed.

FFMPEG
  ffmpeg.exe and ffprobe.exe do the actual encoding work.
  NVENCForge looks for them in this order:
    1. right next to NVENCForge.exe
    2. if they are not there: it downloads a tested build by
       itself (a stable FFmpeg release, roughly 80 MB, once)
    3. only if that download fails - no internet, for example -
       it falls back to an FFmpeg from the Windows search path,
       and tells you that it did
  NVENCForge deliberately prefers its own copy. Every quality
  value it works with was measured against a known build, and a
  different FFmpeg can behave differently - so an unknown one is
  a last resort, not the first choice.
  Want to use your own build? Just put ffmpeg.exe and ffprobe.exe
  next to the exe: a local copy always wins and nothing is
  downloaded. It needs the "libvmaf" filter for the automatic
  quality search - the downloaded build always has it.
  The settings panel shows which one is in use ("own copy" or
  "from PATH").

CONFIGURATION
  All settings live in "NVENCForge_Config.ini" next to the exe,
  created on first run. You do not have to change anything in it.
  The file has two parts: PART 1 holds the handful of settings
  people actually change (target resolution, quality target, audio
  bitrate, what happens to the original, which encoder). PART 2
  holds the expert settings, which are already set to measured
  values. Every single entry explains what it does and which
  values are allowed.
  Invalid values are reported at startup and reset to their
  default in the file individually; your other settings and your
  comments stay untouched.

  Two keys steer speed without touching quality:
    "gpuDecode"  decodes on the GPU (NVDEC) instead of the CPU —
                 about 20% faster on 4K sources at a bit-identical
                 picture, because the decoding step is defined
                 exactly by the codec standard. A decoder error
                 falls back to the CPU automatically. Sources above
                 "gpuDecodeMaxMbit" (default 100) always stay on the
                 CPU: extreme-bitrate HEVC has crashed display
                 drivers, and no fallback can catch that.
    "casStrength" set to 0 skips the sharpening pass after the
                 downscale entirely. That is the single most
                 expensive filter step, so turning it off speeds
                 4K conversions up noticeably — the picture just
                 gets slightly softer. Set it back to 0.4 (or any
                 value up to 1.0) if you prefer the sharper look.

OUTPUT & REQUIREMENTS
  Output folder:    output (next to the processed files)
  Originals go to:  originals (next to the processed files)
  System:           Windows 10/11 x64. For encoding on the graphics
                    card: an Nvidia GPU (Maxwell or newer) with
                    up-to-date drivers.
                    No Nvidia card? Everything still works: -cpu
                    encodes on the processor, and the -davinci,
                    -split and -join tools never needed a GPU
                    in the first place.

  Press Ctrl+C during a conversion to stop; the partial result is
  saved as a playable ".preview.mkv" instead of being discarded.
`

// syncHelpFile writes the user-facing manual next to the executable and
// refreshes it whenever its content differs from this build's text — before
// 1.2.1 an existing file was never touched, so the manual silently went
// stale with every update. The file is generated output, not a user
// document; keeping it current outweighs preserving hand edits. Written
// only on an actual difference (no disk write on a normal start).
// Non-fatal on error: the tool runs regardless.
func syncHelpFile() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("Help.go: syncHelpFile: %w", err)
	}
	path := filepath.Join(filepath.Dir(exePath), helpFileName)
	if old, readErr := os.ReadFile(path); readErr == nil && string(old) == helpFileContent {
		return nil
	}
	if err := os.WriteFile(path, []byte(helpFileContent), 0o644); err != nil {
		return fmt.Errorf("Help.go: syncHelpFile: %w", err)
	}
	return nil
}

// printFirstRunWelcome greets the user on the very first start — recognised by
// the config file not existing yet, so it is shown exactly once per install.
// Its whole job is to defuse the "do I have to set all this up now?" reflex
// that two unfamiliar files appearing next to the exe otherwise trigger.
func printFirstRunWelcome() {
	fmt.Println()
	fmt.Println("  " + pterm.LightCyan("Welcome!") + " NVENCForge just set itself up next to the exe:")
	fmt.Println("    " + pterm.LightYellow(fmt.Sprintf("%-24s", helpFileName)) +
		pterm.Gray("the full manual"))
	fmt.Println("    " + pterm.LightYellow(fmt.Sprintf("%-24s", "NVENCForge_Config.ini")) +
		pterm.Gray("every setting, explained line by line"))
	fmt.Println()
	fmt.Println("  " + pterm.Gray("You do not have to change either of them. The preset values are the"))
	fmt.Println("  " + pterm.Gray("ones this tool was tuned with — good for everyday use as they are."))
	fmt.Println()
}

// helpFlags are the spellings a user might reach for when looking for the
// option list. Windows users routinely type "/?", people coming from Unix
// tools type "--help" — accepting all of them costs nothing and avoids the
// worst first impression a tool can make: answering "unknown option".
var helpFlags = []string{"-help", "--help", "-h", "-?", "/?"}

// wantsHelp reports whether any argument asks for the option list. It scans
// every argument, not just the first: "NVENCForge.exe video.mp4 -help" is a
// perfectly reasonable thing to type when you are stuck mid-command.
func wantsHelp(args []string) bool {
	for _, arg := range args {
		for _, flag := range helpFlags {
			if strings.EqualFold(arg, flag) {
				return true
			}
		}
	}
	return false
}

// printConsoleHelp writes the one-screen option overview that -help shows.
//
// It deliberately lists EVERY option: someone typing -help wants the complete
// picture, not a teaser. What keeps it readable is the one-line-per-option
// limit — the long explanations live in NVENCForge_Help.txt, which this text
// points to at the end. The startup box, by contrast, shows only the three
// options most people ever need.
func printConsoleHelp() {
	const flagColumnWidth = 22

	section := func(title string) {
		fmt.Println()
		fmt.Println("  " + pterm.LightCyan(title))
	}
	// Padding happens BEFORE colouring: pterm wraps the text in ANSI escape
	// sequences, and those invisible characters would otherwise be counted by
	// the field width and shift every description sideways.
	option := func(flags, description string) {
		fmt.Printf("    %s%s\n",
			pterm.LightYellow(fmt.Sprintf("%-*s", flagColumnWidth, flags)),
			description)
	}
	plain := func(left, description string) {
		fmt.Printf("    %s%s\n",
			pterm.LightWhite(fmt.Sprintf("%-*s", flagColumnWidth, left)),
			pterm.Gray(description))
	}

	fmt.Println()
	fmt.Println("  " + pterm.LightWhite(pterm.Bold.Sprint("NVENCForge v"+appVersion)) +
		pterm.Gray("  -  H.265 / AV1 batch converter for Nvidia GPUs"))

	section("USAGE")
	fmt.Println("    " + pterm.LightWhite("NVENCForge.exe [options] [files or folders]"))
	plain("NVENCForge.exe", "converts every video in the current folder")
	plain("drag files onto it", "the same thing - nothing to type at all")

	section("OPTIONS")
	option("-original, -orig", "keep the source resolution (no downscale to 1080p)")
	option("-copyaudio, -ca", "copy all audio tracks 1:1 (no AAC re-encode)")
	option("-av1", "encode AV1 instead of H.265 (needs an RTX 40 or newer)")
	option("-mp4", "write a .mp4 that plays almost everywhere, instead of .mkv")
	option("-8bit", "encode in 8 bit for older devices that reject 10 bit")
	option("-cpu", "encode on the processor - no Nvidia card needed, slower")
	option("-cq NN", "force a fixed quality (H.265 1-51, AV1 1-63; lower = better)")
	option("-noautocq", "switch the automatic quality search off for this run")
	option("-autocq", "switch it on for this run (it is already on by default)")
	option("-NNNN", "maximum bitrate in kbit/s, e.g. -10000")
	option("-keep", "leave the originals exactly where they are")
	option("-shutdown", "shut the PC down 30 s after the batch (\"shutdown /a\" cancels)")

	section("MODES") // must be the first argument - runMode dispatch happens on os.Args[1]
	option("-davinci <files>", "DaVinci Resolve workflow: split, extract, merge, AAC")
	option("-split <files>", "lossless split into video, audio and subtitle files")
	option("-join <files>", "lossless join back into a single .mkv")
	fmt.Println()
	fmt.Println(pterm.Gray("    A mode must be the FIRST argument, e.g.:  NVENCForge.exe -split movie.mkv"))

	section("HELP")
	option("-help, -h, -?, /?", "this list")
	plain("NVENCForge_Help.txt", "the full manual, written next to the exe")
	plain("NVENCForge_Config.ini", "all settings, with an explanation per entry")
	fmt.Println()
}
