<div align="center">

# 🎬 NVENCForge

### Drop a video, get it back smaller: quality-tuned GPU encoding, drag-and-drop.

**H.265 / AV1 NVENC batch encoder with a DaVinci Resolve workflow and lossless split/join, for Windows.**
HDR-aware. Resilient. DaVinci-Resolve-ready. One EXE.

*Powered by FFmpeg, which does the actual encoding. NVENCForge is the automation, validation and safety layer around it.*

[![Windows x64](https://img.shields.io/badge/Windows-10%2F11%20x64-0078D6?logo=windows)](#-requirements)
[![NVIDIA NVENC](https://img.shields.io/badge/GPU-NVIDIA%20NVENC-76B900?logo=nvidia)](#-requirements)
[![AV1 Ready](https://img.shields.io/badge/AV1-RTX%2040%2B-orange)](#-av1-mode-ready-for-the-future)
[![Written in Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?logo=go)](#-building-from-source)
[![License](https://img.shields.io/badge/License-PolyForm%20Noncommercial-blue)](#-license)
[![Ko-fi](https://img.shields.io/badge/Ko--fi-Support%20me-FF5E5B?logo=kofi&logoColor=white)](https://ko-fi.com/burnersen)

**[⬇️ Download the latest release](https://github.com/burnersen/NVENCForge/releases/latest)** · **[☕ Buy me a coffee](https://ko-fi.com/burnersen)**

<img src=".github/screenshot.png" alt="NVENCForge converting a 4K HDR clip with -keep: 266 MB in, 59 MB out, HDR detected and passed through, original kept" width="840">

</div>

---

## ⚡ 30 seconds, no manual

1. Download `NVENCForge.exe`, a single file with nothing to install.
2. Drag a video (or a whole folder) onto it.
3. Done. Your video is now H.265, smaller, and the original sits safely in the recycle bin.

On first run NVENCForge fetches FFmpeg automatically: no setup, no PATH fiddling, no dependencies.

**Some real numbers** from 12 mixed 4K HDR test files on an RTX 5070 Ti, run with `-original -copyaudio` (original 4K resolution kept, audio copied 1:1, so every saved megabyte comes from the video encode alone):

| Source | Before | After | Saved |
|---|---|---|---|
| 400 Mbit/s HEVC 4K demo | 1 435 MB | 65 MB | **−96 %** |
| HDR10+ / Dolby Vision sample | 510 MB | 107 MB | **−79 %** |
| DTS:X IMAX 4K clip | 383 MB | 129 MB | **−66 %** |
| **Whole batch (12 files)** | **5.4 GB** | **0.9 GB** | **−4 481 MB in 2:58 min** |

A reality check on these figures: the −96 % case is a best case, a short clip with an absurdly high source bitrate, and most of that saving comes from the source being wildly inefficient, not from magic. Typical, already-compressed material shrinks far less, and some files get skipped or remuxed entirely because re-encoding wouldn't help. That skip logic is a feature, not a shortcoming. The encoder is CQ-based (constant quality) in every mode: the size shrinks to whatever the chosen quality level needs. In the default mode (no flags) material above 1080p is also downscaled to 1080p.

> **A word of honesty:** NVENCForge re-encodes, and re-encoding is lossy. It shines on bulky, already-compressed or inefficient files where the space saving is worth a quality hit you won't notice in normal playback. It is **not** an archival tool: keep untouched masters of anything irreplaceable. Originals go to the recycle bin (recoverable), not a permanent delete, but treat that as a safety net, not a backup.

---

## ✨ What NVENCForge does

- 🧠 **Smart, not brute-force.** Probes every file first: already-efficient videos are remuxed or skipped instead of re-encoded. Quality is constant (CQ), and a per-file bitrate cap derived from the source keeps every re-encode reliably **smaller than the original** — never bigger, with no fixed-bitrate butchering.
- 🎚️ **Auto-CQ — the right quality level, measured per file** *(new)*. Instead of one fixed CQ for everything, each file gets a quick VMAF-measured analysis that finds the quality level it actually needs — and it's honest about sources that are already compressed to death. Enabled by default; details in [Auto-CQ](#-auto-cq-measured-quality-per-file).
- 🌈 **HDR-aware.** HDR10 (PQ) and HLG are detected. The color tags (transfer, primaries, BT.2020, range) are copied straight from the source, never fabricated. In `-original` mode (no rescale) the static HDR10 mastering-display / MaxCLL metadata rides through as well. When downscaling to 1080p (the default mode) the output stays correctly HDR-tagged (PQ / BT.2020, not washed out), but the static mastering metadata may not survive every FFmpeg build. NVENCForge deliberately never synthesizes HDR metadata values, because a fabricated value is exactly what has broken HDR conversions in the past.
- 🛡️ **Safe with your files.** Originals go to the **recycle bin** only after the output is probed and validated, never hard-deleted. Existing files are never overwritten (automatic numbered names). Abort mid-encode? You keep a playable `.preview.mkv`.
- 🚦 **Resilient by design.** Per-file locks, stall watchdog (kills frozen FFmpeg after 5 min), bounded memory, multi-stage fallback cascade (subs → no subs → AAC → video-only) so one broken stream doesn't take down the whole batch.
- 👯 **Parallel out of the box.** Start the same command in two terminals; instances lock files individually and split the work automatically.
- 🎛️ **DaVinci-Resolve-safe audio.** DTS, TrueHD, EAC3, FLAC, Opus & >5.1 layouts are converted to AAC that Resolve actually imports (≤5.1, ≤48 kHz), or kept 1:1 with `-copyaudio`.
- 🔁 **Ships with its own source.** The EXE carries its own source code (Go `embed`) and extracts it on first run: the source it was built from is right there inside the binary.
- 🌍 **Unicode-safe filename cleanup.** `Movie (2016) [BluRay] x264.mkv` → `Movie.2016.h265.mkv`. Every script in the world survives, release-group noise doesn't.

---

## 🎯 Will my files actually get smaller?

Short answer: **yes — and never bigger.** Before touching anything, NVENCForge reads each file and picks one of two paths:

- **Worth re-encoding?** It shrinks the video at a constant quality level, with a safety cap calculated from the source's own bitrate (it aims for clearly below the original). So a real conversion is reliably **smaller than the source** — and if a result ever came out bigger, it's thrown away automatically.
- **Already lean?** Some files are so efficiently compressed that re-encoding would only make them *bigger* (yes, that really happens). NVENCForge spots this up front and simply repackages the file in seconds instead of wasting minutes of GPU time on a pointless encode.

You can tell the two apart at a glance by the filename:

| Output name | What happened |
|---|---|
| `Movie.h265.mkv` | Re-encoded to H.265 (smaller) |
| `Movie.h264.mkv` | Left in its codec, just repackaged (already efficient) |

Already-processed files are recognized by name and content, so running NVENCForge twice on the same folder never converts anything a second time.

---

## 🚀 Usage

```
NVENCForge.exe [flags] [files/folders]
NVENCForge.exe -davinci [files]
NVENCForge.exe -split [files/folders]
NVENCForge.exe -join [video + audio/subtitle files]
```

| Flag | Effect |
|---|---|
| *(none)* | Convert every supported video in the current folder |
| `-NNNN` | Max target bitrate in kbps (e.g. `-10000`) |
| `-orig` / `-original` | Keep original resolution (no 1080p downscale), raised bitrate cap |
| `-copyaudio` / `-ca` | Copy all audio 1:1, no AAC re-encode |
| `-av1` | Encode **AV1** instead of H.265 (RTX 40+) → `.av1.mkv` |
| `-autocq` | Pick the CQ automatically per file via VMAF measurement — **enabled by default**, works for H.265 and AV1, see [Auto-CQ](#-auto-cq-measured-quality-per-file). Set `autoCQ=false` in the config to turn it off |
| `-noautocq` | Disable Auto-CQ for this run (overrides the `autoCQ=true` config default) |
| `-cq NN` | Force a fixed CQ for this run: skips Auto-CQ and the configured CQ (scale H.265 1-51, AV1 1-63) |
| `-keep` | Keep the originals: don't move them to the recycle bin after a successful convert |
| `-shutdown` | Shut the PC down 30 s after the batch finishes |
| `-davinci` | For DaVinci Resolve workflow (split / extract / merge, re-encodes where needed); must be the first argument |
| `-split` | Lossless split: every stream copied 1:1 into separate files; must be the first argument |
| `-join` | Lossless join: recombine a silent picture + audio/subtitle files into one MKV (1:1); must be the first argument |

Flags combine freely: `NVENCForge.exe -av1 -original -copyaudio -shutdown Movie.mkv`

Supported input: `mp4 mkv ts avi mov flv wmv webm m4v mts m2ts`

### 💡 Pro tip: put NVENCForge into your right-click "Send to" menu

This is my personal workflow: pure drag & drop, no command line. Everyone has their own way; this one has served me well:

1. Keep `NVENCForge.exe` in a folder where you have **write access** (e.g. `Documents\NVENCForge`, *not* `C:\Program Files`; the tool is portable, needs no admin rights and keeps its config right next to the EXE).
2. Press `Win+R`, type `shell:sendto`, press Enter, and your "Send to" folder opens.
3. Create shortcuts to `NVENCForge.exe` in there, one per favorite mode, numbered so they sort nicely. Append the arguments at the end of the *Target* field (shortcut → Properties):

   | Shortcut name | Arguments (after the EXE path) |
   |---|---|
   | `1 NVENCForge Convert 1080` | *(none, default mode)* |
   | `2 NVENCForge Original Copyaudio` | `-original -copyaudio` |
   | `3 NVENCForge AV1 Original` | `-av1 -original` |
   | `4 NVENCForge DaVinci` | `-davinci` |

4. **Important:** clear the **"Start in"** field of every shortcut; it must be **empty**, otherwise "Send to" won't work correctly.

From then on: select any videos → right-click → *Send to* → pick a mode. Done.

---

## 🎚️ Auto-CQ: measured quality, per file

*New in v1.2.* Every video compresses differently: one file looks perfect at CQ 30, another needs CQ 24 for the same visual quality. A single fixed quality level is always a compromise — too generous for easy material (wasted megabytes), too optimistic for hard material. **Auto-CQ replaces that guesswork with an actual measurement**, and it is enabled by default.

Before each encode, NVENCForge runs a short per-file analysis (typically well under a minute, even for a two-hour movie):

1. **Scan.** The source's bitrate profile is read without decoding, and a few short sample windows are placed on the demanding scenes — the hardest scene is always included, so easy scenes can't paint a rosy picture.
2. **Probe.** Those windows are test-encoded at two anchor quality levels with *exactly* the settings of the real encode, and each result is scored with **VMAF** (a perceptual video-quality metric developed by Netflix, 0–100, where ~95+ is visually transparent to most viewers).
3. **Pick & verify.** From the two anchor scores the CQ that hits the quality target (default: VMAF 97) is derived — and then confirmed with one more real measurement. If the verification misses, the pick is corrected. No blind trust in interpolation.

Auto-CQ is also honest about its limits: on heavily pre-compressed sources the reachable quality **saturates** below the target — no CQ can restore detail that is already gone. Instead of pointlessly escalating to expensive quality levels, Auto-CQ detects the plateau and picks the cheapest CQ that still delivers the reachable maximum, probing even higher CQ levels when the quality curve is flat, purely for extra savings.

Tuning knobs in `NVENCForge_Config.ini`:

| Key | Default | Meaning |
|---|---|---|
| `autoCQ` | `true` | Auto-CQ as the startup default (off = classic fixed `targetCQ`) |
| `autoCQTargetVMAF` | `97` | The quality target of the search (70–99) |
| `autoCQTolerance` | `0.5` | May land up to this far below the target when that saves a CQ step → smaller files; `0` = exact targeting |

For a single run: `-noautocq` skips the analysis, `-cq NN` forces a fixed level. Auto-CQ works for both H.265 and AV1 — each on its own VMAF-calibrated CQ scale — and needs an FFmpeg build with `libvmaf` — the automatically downloaded build has it.

---

## 🔮 AV1 mode: ready for the future

`-av1` switches the encoder to **av1_nvenc** (RTX 40 series or newer). [Auto-CQ](#-auto-cq-measured-quality-per-file) now runs here too *(new in v1.3)* and measures the right AV1 CQ per file — av1_nvenc uses its own 1–63 scale, so its anchors were VMAF-calibrated separately; with Auto-CQ off, `av1TargetCQ` is the fixed fallback. AV1 reaches H.265 quality at noticeably smaller sizes thanks to lower bitrate caps. 10-bit and HDR pass-through included. H.265 stays the default; AV1 is strictly opt-in.

> **Black video when playing AV1?** Your player, not your file. In MPC-HC/LAV Filters set *Hardware Decoder* to **D3D11 with device "Automatic"** or **DXVA2 (native)**; the copy-back path of older configs shows black video on 10-bit AV1. Windows Media Player needs the free *AV1 Video Extension* from the Microsoft Store. Note: Apple TV has no AV1 hardware decoding yet.

---

## 🧰 For DaVinci Resolve Workflow (`-davinci`)

| You drop… | You get… |
|---|---|
| One or more `.mkv` | Silent `.NoSound.mp4` (stream copy) + each audio track as `.m4a`/`.wav` + cleaned `.srt`/`.sup`/`.idx` subtitles |
| `.mp4` / `.mov` / `.m4v` | Silent `.NoSound.mp4` + separated audio & subtitle tracks |
| One video + audio/subtitle files | A finished `.sub.mkv` with correct language tags, default flags, forced/SDH dispositions |
| **Nothing** (just `-davinci`) | **Batch mode:** every MKV in the folder is split automatically, with no prompts, parallel-instance safe |

Track selection is interactive (multichannel audio offers an optional stereo downmix), languages are auto-detected from filenames (`Movie.de.srt` → German). Every extracted SRT is cleaned automatically: HTML/ASS tags, invisible Unicode characters and ad phrases removed (configurable via `SRTCleaner_config.txt`).

### The DaVinci Resolve workflow

1. **Split** your source MKV → lightweight silent MP4 + separate audio stems (all Resolve-compatible).
2. **Edit/grade** in Resolve; import just works, including 5.1 audio.
3. **Export** your master from Resolve (map each timeline track to its own output track).
4. **Merge** the master MP4 + original audio/subs back into a distribution MKV with one drag & drop.

---

## 🪓 Lossless Split / Join (`-split` / `-join`)

Where `-davinci` re-encodes incompatible audio to AAC and converts/cleans subtitles for editing, `-split` and `-join` never touch the data: **every stream is copied 1:1**, no re-encode, no cleaning. A `-split` followed by a `-join` is a true lossless round-trip.

| You run… | You get… |
|---|---|
| `-split` on one file | A prompt to pick tracks (Enter = all), then a silent `.NoSound` picture (`mp4`/`mov` and transport streams like `.ts`/`.m2ts` become `mp4`, everything else → `mkv`), each audio track in its **native** container (`.ac3` `.dts` `.eac3` `.m4a` `.flac` `.thd` `.mka` …) and each subtitle **untouched** (`.srt` `.ass` `.sup` `.idx` …) |
| `-split` on a folder, or nothing | **Batch mode:** every supported video split automatically, all tracks, no prompts, parallel-instance safe |
| `-join` on a silent video + audio/subtitle files | One `.joined.mkv` with everything copied 1:1, German audio set as default, languages and forced/SDH flags read from the filenames |

The silent picture always gets a `.NoSound` suffix, so the original is never overwritten. The stereo-downmix option from `-davinci` is hidden in `-split`, because a downmix would be a re-encode.

**On join, only the picture of the base is used.** `-join` takes just the video track from the base file (the silent `.NoSound` picture); any audio or subtitles the base might still carry are simply ignored, never merged in. Your source files are never modified, so nothing is lost — you choose the audio and subtitle files you actually want as the other arguments. Because every stream is copied 1:1, picture and sound stay in sync; a `-split` followed by `-join` is a clean lossless round-trip.

---

## ⚙️ Configuration

Everything lives in `NVENCForge_Config.ini` next to the EXE (auto-created; invalid values are reset to their default in the file individually with a warning, all valid settings left untouched):

CQ quality level, Auto-CQ (on/off, VMAF target, tolerance), bitrate caps (H.265 and AV1 separately), resolution cap, NVENC preset/lookahead/B-frames, CAS sharpening, AAC bitrates, auto-shutdown, extra filename characters.

---

## 💻 Requirements

- Windows 10/11 x64
- NVIDIA GPU with NVENC (Maxwell or newer); **RTX 40+ for AV1**
- The `-davinci`, `-split` and `-join` modes run on **any** hardware (no GPU needed)
- FFmpeg: downloaded automatically on first run (or drop your own `ffmpeg.exe`/`ffprobe.exe` next to the EXE)

> **Why NVENC and not x265?** Hardware encoding trades a little compression efficiency for a huge speed gain and leaves your CPU free. For batch-crushing a large library that tradeoff is the whole point; if you want the absolute best bytes-per-quality on a single precious file, a slow x265 CPU encode will still beat it. NVENCForge is built for throughput and safety, and tuned (CQ + AQ + lookahead + multipass) to keep the quality hit small.

> **Why CPU decoding?** Decoding runs deliberately on the CPU and only encoding on the GPU: extreme-bitrate HEVC sources (400 Mbit/s+) can crash GPU drivers (TDR) when hardware-decoded. NVENCForge chooses stability over decode speed, verified on real files.

---

## 🛡️ Windows SmartScreen / antivirus warnings

Windows or your antivirus may warn you the first time you run `NVENCForge.exe`. Here's the honest reason: **the EXE is not code-signed.** Signing certificates cost several hundred euros *per year*, and this is a free hobby project with zero income, so that's not happening. Unsigned Go binaries are frequent false-positive targets; there is nothing I can do about it except be transparent.

You don't have to trust me blindly:

- **Scan it:** upload the EXE to [VirusTotal](https://www.virustotal.com) before running it.
- **Read it:** the complete source code is right here in this repository, every line.
- **Build it yourself:** clone, run `build.bat`, done. (The downloaded EXE even carries its own source inside and extracts it on first run; the source it was built from ships with it.)

If SmartScreen blocks the start: click **"More info" → "Run anyway"**.

---

## 🔨 Building from source

```
cd sourcecode
build.bat
```

That's it. `build.bat` packs the embedded source archive and compiles `NVENCForge.exe` (Go 1.21+). And remember: every released EXE extracts its own sources on first run.

---

## 🔧 Under the hood — the safety nets and clever bits

Most of the work in NVENCForge isn't the encoding itself — FFmpeg does that — it's everything *around* it: keeping your files safe, keeping a batch from ever getting stuck, and giving each file exactly the treatment it needs. Below is the complete list, grouped by what it's for. It's deliberately thorough (this is the nerdy part of the page); every point is real, working behaviour. **Click any section to expand it.**

<details>
<summary><b>🛡️ Your files are safe — no matter what</b></summary>

- **Validate, *then* recycle.** An original is only moved to the recycle bin *after* the new file has been re-probed and confirmed valid (right codec, no lost audio tracks, plausible duration, sane file size). If validation fails, the original stays exactly where it is.
- **The real Windows recycle bin.** Deletion goes through the actual Windows shell API with the "allow undo" flag, so originals are restorable — not a custom "move to a folder" hack. It even detects a drive that has no recycle bin (instead of silently failing) and tells you the original was kept.
- **Never overwrite anything.** If an output name is already taken, NVENCForge writes an automatic numbered name (`Movie.2`, `Movie.3`, …). Nothing you already have is ever clobbered.
- **A re-encode is never bigger than the source.** If a conversion somehow comes out larger (it happens on already-lean files), the result is thrown away and the file is losslessly repackaged instead. You never trade quality *and* gain size.
- **Crash-safe output.** Abort mid-encode, close the window, or lose power — you're left with a playable `.preview.mkv`, never a corrupt zero-byte file. FFmpeg is asked to *finish cleanly* (a graceful "q"), not killed mid-frame.
- **Video-only fallback keeps the original.** If the only way to salvage a file is to drop its audio, the source — the only copy of that audio — is deliberately **not** recycled.
- **A corrupt output can never masquerade as a good one.** If a broken result can't be deleted, it's renamed `.broken`, and if even that fails a `.invalid` marker is dropped next to it, so a later run treats it as garbage instead of "already done". Half-written files from an earlier crash ("crash ghosts") are detected and cleared too.
- **Won't convert the same file twice.** Every file NVENCForge *produces* gets a small origin tag in its header (`NVENCFORGE_SOURCE` — just the source's name; it never touches the picture, and your untouched originals get no tag at all). Before working, NVENCForge looks in the `output` folder and skips anything already finished for that source and codec: a re-run simply does nothing, an `.h265` and an `.av1` of the same film happily coexist, and two *different* sources that clean to the same name are told apart instead of one shadowing the other. The skip depends only on whether a finished output already exists — not on flags like `-cq` or a bitrate override — so to deliberately redo a file with new settings, remove its finished file from the `output` folder first.
- **Keeps the original's date.** The output inherits the source file's creation and modification timestamps, so your library sorts by date exactly as before.

</details>

<details>
<summary><b>🎯 Right size, right quality — the sizing intelligence</b></summary>

- **Probe first, then decide.** Every file is read up front. Already-efficient videos are repackaged in seconds instead of wasting GPU minutes on a re-encode that couldn't help — and you can tell which happened at a glance from the output name (`.h265` = re-encoded, `.h264` = just repackaged).
- **Constant quality, not fixed bitrate.** The encoder targets a *quality* level (CQ), so a file shrinks to whatever that quality actually needs — no crude fixed-bitrate butchering that starves hard scenes and wastes bits on easy ones.
- **A bitrate cap derived from the source.** On top of CQ, a safety ceiling is computed from the file's *own* bitrate (aiming well below it), clamped up to a per-resolution floor (so low-bitrate sources don't turn to mush) and down to a per-mode ceiling. That's what guarantees a real conversion lands smaller than the source. An explicit `-NNNN` always wins over both.
- **Honest bitrate estimation.** To judge "video vs. audio" it subtracts a per-codec audio estimate (TrueHD, DTS, FLAC, PCM, AC3, EAC3 all weighted differently) from the total, and prefers the precise per-stream figure when the container reports a trustworthy one.
- **Clean metadata.** Stale per-track statistics tags that other muxers leave behind (which make MediaInfo show absurd bitrates) are stripped from every output.

</details>

<details>
<summary><b>🌈 Picture, HDR & colour done right</b></summary>

- **HDR detected by the right signal.** HDR10 (PQ) and HLG are recognised by the *transfer function*, not just BT.2020 primaries — so plain wide-gamut SDR isn't misread as HDR.
- **Colour is copied, never invented.** Transfer, primaries, colour space and range are passed through 1:1 from the source, and obviously-bogus tags are skipped rather than propagated. Static HDR10 mastering-display / MaxCLL metadata rides along automatically on stream-copy and on `-original` re-encodes. NVENCForge flatly refuses to *synthesize* an HDR value — a fabricated one is exactly what has broken HDR conversions in the past.
- **True 10-bit pipeline.** Everything runs in 10-bit (`p010le`, HEVC Main-10), so banding in skies and gradients doesn't get worse.
- **Automatic deinterlacing.** TV-style interlaced sources are detected from their field order and deinterlaced with `bwdif` *before* any scaling (keeping the original frame rate), so old recordings come out clean.
- **Careful downscaling.** The 1080p downscale preserves aspect ratio, forces even dimensions (odd ones break encoders), and follows up with a light contrast-adaptive sharpen (CAS) to counter the softness scaling introduces. Even in no-scale mode dimensions are evened out.
- **Encoder tuned for quality-per-bit.** VBR + CQ, `tune hq`, spatial **and** temporal adaptive quantisation, multi-pass, look-ahead, B-frames with a pyramid reference, constant frame rate, and a keyframe interval sized to ~4 seconds of video — the whole reason the quality hit stays small at hardware-encode speed.

</details>

<details>
<summary><b>🎚️ Auto-CQ — measuring quality instead of guessing</b></summary>

The [Auto-CQ section above](#-auto-cq-measured-quality-per-file) covers *what* it does; here's *how* it stays honest, for the curious:

- **Sample encodes use the real settings.** The little test clips are encoded with the *exact* encoder options of the final run, and the reference side runs through the *same* downscale/sharpen filter chain — so the VMAF score isolates the encoder's loss alone, not the scaling.
- **Finds the hard scenes without decoding.** Sample windows are placed using the source's bitrate profile, read straight from the container by demuxing packet *sizes* (no decoding — seconds even on a multi-GB movie). The single heaviest scene is always included; intros, credits and near-black frames (which score a flattering fake-perfect VMAF) are deliberately avoided.
- **Two nasty VMAF pitfalls handled.** Decoded segments are re-based to a zero start time, and both comparison inputs are forced onto frame-number-based timestamps — otherwise Matroska's millisecond rounding pairs the wrong frames and tanks the score. (These are the kind of bugs that silently make a quality measurement meaningless.)
- **Trust, but verify.** The interpolated pick is always confirmed with one extra real measurement; on a miss it steps down along the measured slope. A **saturation brake** detects pre-compressed sources whose quality plateaus below the target and picks the *cheapest* level on that plateau instead of pointlessly burning bitrate; a **plateau climb** even probes higher CQ levels when the curve is provably flat, purely for extra savings.
- **It can never break a conversion.** Any hiccup (clip under 30 s, unknown frame rate, an FFmpeg build without `libvmaf`, a wedged step) just falls back to the configured CQ with a warning. The whole analysis runs at idle priority with hard per-step timeouts, and `libvmaf`'s presence is checked once up front — one clear notice, not one failure per file.
- **Calibrated per codec.** H.265 and AV1 each search on their own VMAF-calibrated CQ scale, because the same number means very different quality on the two encoders.

</details>

<details>
<summary><b>🚦 It won't fall over — robustness during a batch</b></summary>

- **A GPU check that matches reality.** At startup NVENCForge does a dummy encode with the *exact* flags the real encode uses — not a lighter test — so a card that would fail later is caught now. Older pre-Turing cards (Pascal/Volta) are retried once in a degraded mode and then run *without* the advanced features instead of failing on every single file; AV1 gets its own separate probe.
- **CPU decoding on purpose.** Decoding stays on the CPU because hardware-decoding extreme-bitrate HEVC (400 Mbit/s+) can crash the GPU driver (a TDR reset). Stability beats decode speed — verified on real files.
- **Multi-stage fallback cascade.** If one stream in a file is broken, NVENCForge walks down a ladder (keep subtitles → drop subtitles → re-encode audio to AAC → video-only), and the FFmpeg error text steers it straight to the rung that can actually fix the problem instead of retrying dead ends. One bad stream doesn't sink the batch.
- **Stall watchdog.** A frozen FFmpeg is detected and stopped after 5 minutes of silence so the batch moves on — no hanging forever on one stuck file. (Auto-CQ steps and probes each carry their own hard timeouts too.)
- **Parallel-safe by design.** Start the same command in two terminals and they split the work automatically. Each file is locked with a small JSON lock that records the process, machine and start time; a lock whose owner process has died is reclaimed (checked by real process ID *and* image name, so a recycled PID can't fool it), while a lock owned by *another machine* on a shared drive is never stolen.
- **Self-healing config.** An invalid value in `NVENCForge_Config.ini` is reset to its default *in the file itself* — only that one line, leaving your comments, valid values and unknown keys untouched — so the same warning doesn't nag you every run.
- **Hardened downloader.** The first-run FFmpeg download has real timeouts on every stage (connect, handshake, response, idle) plus an overall cap, so a dead connection can't freeze the app; it streams to a temp file and extracts only `ffmpeg.exe`/`ffprobe.exe`.
- **A pinned, stable FFmpeg.** The download deliberately picks the newest *stable release-branch* build, not the bleeding-edge master — dev builds have silently renamed or dropped encoder options before, which made the GPU probe fail as if the card itself were broken. If that probe ever fails, the underlying FFmpeg error is always shown so a bad build isn't mistaken for a missing GPU.
- **Never dies silently.** An unexpected crash is caught and shown in a message that keeps the window open (instead of vanishing), and any per-file failures are collected into an `error_report.txt` next to the files.

</details>

<details>
<summary><b>🎛️ Streams, audio & subtitles (the editing side)</b></summary>

- **DaVinci-Resolve-safe audio.** Formats Resolve chokes on (DTS, TrueHD, EAC3, FLAC, Opus, and anything above 5.1 or above 48 kHz) are converted to AAC it actually imports — including the specific 7.1 case Resolve reads as silence — while already-compatible tracks are copied untouched. `-copyaudio` keeps everything 1:1.
- **Lossless split / join.** `-split` copies every stream 1:1 into its native container (`.ac3`, `.dts`, `.flac`, `.sup`, `.srt`, …) with a silent picture; `-join` muxes them back into one MKV. A split→join round-trip is bit-for-bit lossless, and the timestamps are normalised so the rejoined file stays perfectly in sync.
- **Smart subtitle handling.** Text subtitles are converted to clean SRT; bitmap subtitles (PGS, VobSub) that *can't* become text are copied through untouched. Attachments like embedded fonts and cover art ride along on their own.
- **Automatic SRT cleaning.** Every extracted SRT has its HTML/ASS styling tags, invisible Unicode junk (soft hyphens, zero-width characters, byte-order marks) and advertising lines stripped — the ad-phrase list is yours to edit in `SRTCleaner_config.txt`, and the file is rewritten atomically.
- **Sensible track defaults.** Languages are auto-detected from filenames (`Movie.de.srt` → German, with all the ISO code variants mapped), forced/SDH flags are read from the names, a stereo down-mix is offered as an opt-in extra, and merging uses only the picture of the base file (it asks first if that base still carries its own audio).
- **No GPU needed here.** The `-davinci`, `-split` and `-join` modes are pure remux/stream work and run on any PC, Nvidia card or not — the GPU probe is skipped entirely.

</details>

<details>
<summary><b>🪟 Windows-native craftsmanship (the deep nerdy bits)</b></summary>

- **Live progress you can trust.** A per-file *and* an overall batch bar, with ETA, encode speed, fps, bitrate, frame count — and a continuously-smoothed *projected* output size that already tells you "≈ −60 %" long before the file is done. The cursor is hidden and line-wrap disabled during the render so nothing smears.
- **Stays out of your way.** Every heavy FFmpeg job — the encodes and the Auto-CQ analysis — runs at **idle priority**, so a big batch doesn't make the rest of your PC sluggish, and none of the FFmpeg/FFprobe calls pop up a console window.
- **Real long-path support.** Paths over the classic 260-character limit — and UNC network paths — are handled with the `\\?\` prefix, correctly round-tripped both ways.
- **Colours on old terminals.** ANSI/virtual-terminal mode is switched on explicitly, so the coloured UI works even in the plain classic console.
- **Graceful window-close.** Closing the window, logging off or shutting down is caught: FFmpeg is given a few seconds to finalise the current file into a playable preview instead of leaving a corrupt fragment.
- **Ships its own source, safely.** The binary carries the exact source it was built from and extracts it on first run — but only if the folder isn't already there (your edits are never overwritten), and with a zip-slip guard so a crafted archive can't write outside its folder.
- **Correct to the byte.** The low-level Windows calls (recycle bin, timestamps) are laid out to match the OS ABI exactly, checked *at compile time* — if a structure offset were ever wrong, the build fails instead of shipping a subtle bug.

</details>

<details>
<summary><b>🧩 Convenience & polish</b></summary>

- **One portable EXE.** Nothing to install, no admin rights; it keeps its config, help and tools right next to itself. FFmpeg is fetched automatically on first run.
- **Worldwide filename cleanup.** Release-group noise is stripped (`Movie (2016) [BluRay] x264.mkv` → `Movie.2016.h265.mkv`) while every script and alphabet on earth (CJK, Cyrillic, Greek, Arabic, …) survives intact; useful codec/resolution/HDR tags are kept, Windows-forbidden characters are dropped, and you can whitelist extra characters in the config.
- **Tells you what it'll do — and what it did.** A colour settings panel at startup shows every effective parameter (and highlights whatever a flag just changed); a summary at the end reports converted / skipped / failed counts and the total megabytes saved.
- **Self-updating help.** A plain-text manual is written next to the EXE and refreshed automatically whenever it goes out of date with the build — no stale help after an update.
- **Send-to drag & drop.** Wire it into the Windows "Send to" menu (see [Pro tip](#-pro-tip-put-nvencforge-into-your-right-click-send-to-menu)) and never touch a command line again.
- **Auto-shutdown** for long overnight batches (`-shutdown`, with a 30-second cancel window).

</details>

---

## 🧑‍🎨 The story

NVENCForge is a personal hobby project, built over two months of evenings to fit my own media workflow. Every feature, every workflow rule and all the real-world testing on 4K HDR files came from me. It started as a tool just for myself, but if it fits your workflow too, all the better.

---

## 📜 License

NVENCForge is source-available under the [PolyForm Noncommercial License 1.0.0](LICENSE.md).
Free to use, study, modify and share for any **noncommercial** purpose: personal use, hobby, education, research. **Commercial use, resale or bundling into paid products is not permitted** without a separate license from the author. Want a commercial license? Open an issue or reach out.

### FFmpeg

NVENCForge does **not** bundle FFmpeg. On first run it downloads an official static build from the [BtbN FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) project (GPL-licensed) onto your machine, or you provide your own copy. FFmpeg is a separate work by the [FFmpeg project](https://ffmpeg.org) under its own license; NVENCForge invokes it as an external program. This software uses libraries from the FFmpeg project under the GPL.

---

## 💬 Feedback & contributions

Found a bug or have a feature wish? Please open an issue on the [GitHub repository](../../issues) — feedback is genuinely welcome. Try it out and don't hesitate to post your wishes. Forks and pull requests for noncommercial improvements are welcome too. When reporting a bug, the console output helps (run with `-debug` for details).

---

## ☕ Support

NVENCForge is free and made in my spare time. If it helps you and you'd like to say thanks, you can [buy me a coffee on Ko-fi](https://ko-fi.com/burnersen). Completely optional — and thank you!

---

## ⚠️ Disclaimer

NVENCForge is free hobby software, provided **"as is", without any warranty or condition of any kind**. It was built and tested with care (your originals are never deleted, only moved to the recycle bin after the output has been validated), but you use it **at your own risk**. As far as the applicable law allows, the author is not liable for any damages or data loss arising from the use of this software. See the *No Liability* clause of the [license](LICENSE.md).
