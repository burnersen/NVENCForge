# 🔧 NVENCForge — the technical page

Everything that didn't need to be on the front page: how the modes work in
detail, every configuration key, and the complete list of what happens under the
hood. **[← Back to the README](README.md)**

**Contents**

- [🎚️ Auto-CQ in depth](#auto-cq-depth)
- [🔮 AV1 mode](#av1-depth)
- [📱 MP4 mode and `-8bit`](#mp4-depth)
- [💻 CPU mode](#cpu-depth)
- [🧰 DaVinci Resolve mode](#davinci-depth)
- [🪓 Lossless Split / Join](#split-depth)
- [📡 JSON event channel (`-json`)](#json-events)
- [⚙️ Configuration, key by key](#config-depth)
- [🔧 Under the hood — the safety nets and clever bits](#under-the-hood)
- [🔨 Building from source](#building)

---

<a id="auto-cq-depth"></a>

## 🎚️ Auto-CQ in depth

*New in v1.2.* Every video compresses differently: one file looks perfect at CQ 30, another needs CQ 24 for the same visual quality. A single fixed quality level is always a compromise — too generous for easy material (wasted megabytes), too optimistic for hard material. **Auto-CQ replaces that guesswork with an actual measurement**, and it is enabled by default.

Before each encode, NVENCForge runs a short per-file analysis (typically well under a minute, even for a two-hour movie):

1. **Scan.** The source's bitrate profile is read without decoding, and a few short sample windows are placed on the demanding scenes — the hardest scene is always included, so easy scenes can't paint a rosy picture.
2. **Probe.** Those windows are test-encoded at two anchor quality levels with *exactly* the settings of the real encode, and each result is scored with **VMAF** (a perceptual video-quality metric developed by Netflix, 0–100, where ~95+ is visually transparent to most viewers).
3. **Pick & verify.** From the two anchor scores the CQ that hits the quality target (default: VMAF 96) is derived — and then confirmed with one more real measurement. If the verification misses, the pick is corrected. No blind trust in interpolation.

Auto-CQ is also honest about its limits: on heavily pre-compressed sources the reachable quality **saturates** below the target — no CQ can restore detail that is already gone. Instead of pointlessly escalating to expensive quality levels, Auto-CQ detects the plateau and climbs to cheaper CQ levels that provably stay near the reachable maximum — every candidate is confirmed by a real VMAF measurement, and on such sources the file often shrinks by a third or more at no visible cost.

Tuning knobs in `NVENCForge_Config.ini`:

| Key | Default | Meaning |
|---|---|---|
| `autoCQ` | `true` | Auto-CQ as the startup default (off = classic fixed `targetCQ`) |
| `autoCQTargetVMAF` | `96` | The quality target of the search (70–99) |
| `autoCQTolerance` | `0.5` | May land up to this far below the target when that saves a CQ step → smaller files; `0` = exact targeting |
| `autoCQPlateauTolerance` | `2.5` | Extra savings budget when the target is provably unreachable (pre-compressed sources): the pick may drop up to this many VMAF points below the measured maximum, each step confirmed by a real measurement. The full budget applies only where the measured curve is flat (plateau noise) — a target merely grazed on a steep curve spends at most `autoCQTolerance`; `0` = keep the conservative pick |

For a single run: `-noautocq` skips the analysis, `-cq NN` forces a fixed level. Auto-CQ works for both H.265 and AV1 — each on its own VMAF-calibrated CQ scale — and needs an FFmpeg build with `libvmaf` — the automatically downloaded build has it.

### How it stays honest

- **Sample encodes use the real settings.** The little test clips are encoded with the *exact* encoder options of the final run, and the reference side runs through the *same* downscale/sharpen filter chain — so the VMAF score isolates the encoder's loss alone, not the scaling.
- **Finds the hard scenes without decoding.** Sample windows are placed using the source's bitrate profile, read straight from the container by demuxing packet *sizes* (no decoding — seconds even on a multi-GB movie). The single heaviest scene is always included; intros, credits and near-black frames (which score a flattering fake-perfect VMAF) are deliberately avoided.
- **Two nasty VMAF pitfalls handled.** Decoded segments are re-based to a zero start time, and both comparison inputs are forced onto frame-number-based timestamps — otherwise Matroska's millisecond rounding pairs the wrong frames and tanks the score. (These are the kind of bugs that silently make a quality measurement meaningless.)
- **Trust, but verify.** The interpolated pick is always confirmed with one extra real measurement; on a miss it steps down along the measured slope. A **saturation brake** detects pre-compressed sources whose quality plateaus below the target, and a **plateau climb** then probes higher CQ levels with real measurements — on such sources the low CQ steps often just ride the bitrate cap, so the climb routinely cuts a third of the file size at no visible cost.
- **It can never break a conversion.** Any hiccup (clip under 30 s, unknown frame rate, an FFmpeg build without `libvmaf`, a wedged step) just falls back to the configured CQ with a warning. The whole analysis runs at idle priority with hard per-step timeouts, and `libvmaf`'s presence is checked once up front — one clear notice, not one failure per file.
- **Calibrated per codec.** H.265 and AV1 each search on their own VMAF-calibrated CQ scale, because the same number means very different quality on the two encoders.

---

<a id="av1-depth"></a>

## 🔮 AV1 mode: ready for the future

`-av1` switches the encoder to **av1_nvenc** (RTX 40 series or newer). [Auto-CQ](#auto-cq-depth) runs here too *(new in v1.3)* and measures the right AV1 CQ per file — av1_nvenc uses its own 1–63 scale, so its anchors were VMAF-calibrated separately; with Auto-CQ off, `av1TargetCQ` is the fixed fallback. AV1 reaches H.265 quality at noticeably smaller sizes thanks to lower bitrate caps. 10-bit and HDR pass-through included. H.265 stays the default; AV1 is strictly opt-in.

> **Black video when playing AV1?** Your player, not your file. In MPC-HC/LAV Filters set *Hardware Decoder* to **D3D11 with device "Automatic"** or **DXVA2 (native)**; the copy-back path of older configs shows black video on 10-bit AV1. Windows Media Player needs the free *AV1 Video Extension* from the Microsoft Store. Note: Apple TV has no AV1 hardware decoding yet.

---

<a id="mp4-depth"></a>

## 📱 MP4: the file that plays everywhere

`-mp4` writes an **`.mp4`** instead of the usual `.mkv` — the container almost every device opens: the iOS Photos app, smart TVs, tablets, browsers. Three things normally trip players up, and `-mp4` handles all of them automatically:

- **The codec tag.** HEVC in MP4 must be tagged **`hvc1`** — Apple refuses the `hev1` tag FFmpeg writes by default. (It's also the tag DaVinci Resolve prefers, so nothing is lost.)
- **Container & audio.** The iOS gallery won't accept MKV at all, and only plays **AAC** audio. `-mp4` delivers MP4 with `+faststart` — so playback starts immediately instead of after a seek to the end of the file — and re-encodes non-AAC tracks (AC3/DTS/…) to AAC where needed.
- **Tracks.** MP4 carries several audio tracks just fine, but not every player lets you switch, and every extra track costs space. With more than one audio or subtitle track you are **asked which ones to keep** (no answer within 30 seconds keeps them all). Text subtitles go in as `mov_text`; picture subtitles from Blu-rays cannot be stored in MP4 at all and are reported instead of silently dropped.

A fresh source is encoded to H.265 as usual and then packaged. A file you **already converted** (`.h265.mkv`) is just **repackaged losslessly** — no second encode — and the original MKV is kept. AV1 plays on neither iPhones nor most TVs, so `-mp4` always uses H.265 (an existing `.av1.mkv` is skipped with a hint to re-run `-mp4` on the original source).

> `-apple`, the flag's original name, still works — existing "Send to" shortcuts keep running.

### When a device still refuses: `-8bit`

NVENCForge encodes in **10 bit** by default. That costs nothing with H.265 and avoids the banding you otherwise see in dark gradients. Modern iPhones, iPads and TVs handle it without blinking.

Some older hardware can't: TVs, beamers and Android phones from the 8-bit era show a black screen or reject the file outright. `-8bit` encodes in 8 bit (profile `main`) for exactly those cases. It works with every mode — H.265, AV1, `-cpu`, `-mp4`. Only use it when a device actually refuses to play; 10 bit is the better picture.

> Repackaging an already-converted file can't change its bit depth — that needs a real conversion from the source.

---

<a id="cpu-depth"></a>

## 💻 CPU mode: no NVIDIA card needed

`-cpu` moves the encode from the graphics card to the processor — **libx265** for H.265, **SVT-AV1** together with `-av1`. Everything else stays exactly the same: downscaling, sharpening, HDR handling, audio, bitrate caps, Auto-CQ, file naming, the safety net that discards an encode that came out bigger.

```
NVENCForge.exe -cpu Movie.mkv          # H.265 on the processor
NVENCForge.exe -cpu -av1 Movie.mkv     # AV1 on the processor
```

Set `encoder=cpu` in the config to make it permanent. And if NVENCForge starts on a machine **without** an NVIDIA card, it no longer gives up — it offers CPU mode right there (and continues by itself after 15 s, so unattended batches keep running).

**The honest numbers** *(measured 25 Jul 2026)* on an 8-core Ryzen, 24 s of 1080p material, all encoders compared at equal file size:

| Encoder | Time | Quality at equal size |
|---|---|---|
| NVENC `p5` (GPU) | 8 s | reference |
| libx265 `fast` | 33 s | same as NVENC |
| libx265 `slow` | 106 s | +1.0 VMAF |
| SVT-AV1 preset 6 | 34 s | +0.4 VMAF |

Two things worth knowing before you switch:

- **CPU mode is not a quality upgrade.** At the default preset it lands where your GPU already is — it exists so people *without* an NVIDIA card can use the tool at all. Want more? `cpuPreset=slow` buys ~1 VMAF for triple the time.
- **With `-cpu`, AV1 is the better deal.** SVT-AV1 at preset 6 takes about the same time as libx265 `fast` and still delivers smaller files at equal quality. If the target device plays AV1, use `-cpu -av1`.

Rough throughput: about **40 minutes per hour of 1080p video** on a modern 8-core CPU, clearly more on older machines — and unlike a GPU encode it keeps every core busy. `cpuThreads=8` (or any number) in the config caps that so the machine stays usable while it works.

> The quality values live on their own scales — `cpuTargetCRF` for H.265, `cpuAV1TargetCRF` for AV1. Measured: libx265 needs roughly **CQ minus 7** for the same quality, so NVENC CQ 26 and x265 CRF 19 look alike. Auto-CQ knows this and searches on its own anchors per encoder.

---

<a id="davinci-depth"></a>

## 🧰 DaVinci Resolve: when Resolve won't read your file (`-davinci`)

**Imported a video and got no sound? Your 5.1 track missing? Resolve refusing the MKV entirely?**

Nothing is broken on your end. DaVinci Resolve — Studio included — does not read the audio formats that MKV files routinely carry: **AC3, E-AC3, DTS, TrueHD/MLP, FLAC, Opus and Vorbis**. MKV itself isn't an officially supported container on Windows either, and audio layouts beyond 5.1 or above 48 kHz are refused as well. The usual "fix" is to re-encode the entire film in some converter and hope the result imports.

`-davinci` deals with the actual problem instead. It leaves your **picture untouched** (stream copy — no quality loss, no waiting on an encode) and only takes the audio and subtitles apart: every track that Resolve can't read is converted to AAC it can (≤ 5.1, ≤ 48 kHz), everything already compatible is copied as-is, and subtitles come out as cleaned `.srt`. When you're finished editing, one drag & drop merges your Resolve master back together with the original audio and subtitles.

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

<a id="split-depth"></a>

## 🪓 Lossless Split / Join (`-split` / `-join`)

Where `-davinci` re-encodes incompatible audio to AAC and converts/cleans subtitles for editing, `-split` and `-join` never touch the data: **every stream is copied 1:1**, no re-encode, no cleaning. A `-split` followed by a `-join` is a true lossless round-trip.

| You run… | You get… |
|---|---|
| `-split` on one file | A prompt to pick tracks (Enter = all), then a silent `.NoSound` picture (`mp4`/`mov` and transport streams like `.ts`/`.m2ts` become `mp4`, everything else → `mkv`), each audio track in its **native** container (`.ac3` `.dts` `.eac3` `.m4a` `.flac` `.thd` `.mka` …) and each subtitle **untouched** (`.srt` `.ass` `.sup` `.idx` …) |
| `-split` on a folder, or nothing | **Batch mode:** every supported video split automatically, all tracks, no prompts, parallel-instance safe |
| `-join` on a silent video + audio/subtitle files | One `.joined.mkv` with everything copied 1:1, German audio set as default, languages and forced/SDH flags read from the filenames |

The silent picture always gets a `.NoSound` suffix, so the original is never overwritten. The stereo-downmix option from `-davinci` is hidden in `-split`, because a downmix would be a re-encode.

**On join, dropped audio files replace the base audio.** `-join` takes the video track from the base file plus the audio files you drop; if you drop no audio at all (subtitles only), the base video keeps its own sound instead of going silent. Subtitles inside the base are never carried over — you choose the subtitle files you actually want as arguments. Your source files are never modified, so nothing is lost. Because every stream is copied 1:1, picture and sound stay in sync; a `-split` followed by `-join` is a clean lossless round-trip.

---

<a id="json-events"></a>

## 📡 JSON event channel (`-json`)

A front-end or script should never have to read the screen text to follow a run — the display is written for humans and does change between versions (1.15.0 was a display-only release). With `-json`, NVENCForge splits its two channels:

- **stdout** carries nothing but events, one JSON object per line
- **stderr** carries the complete normal display, unchanged

Both channels still end up next to each other in a terminal, so nothing looks different when you run it by hand.

In this mode the **moving** parts of the display are switched off: the Auto-CQ spinner and the live progress block. They repaint themselves ten times a second, which only works on a real terminal — on a pipe every repaint becomes another line (measured: 169 log lines for one file, 51 with them gone). Everything static stays: settings table, Auto-CQ result, warnings, summary.

| Event | When | Key fields |
|---|---|---|
| `run` | once, before the first file | `version`, `mode` (convert/davinci/split/join), `codec`, `encoder`, `files` |
| `file` | a file starts | `index`, `total`, `name`, `path`, `in_mb` |
| `stage` | the step changes | `stage`: `analyze` \| `encode` \| `remux` \| `mp4` |
| `progress` | ~10× per second while encoding | `pct`, `pos_sec`, `eta`, `fps`, `speed`, `bitrate`, `est_mb` |
| `result` | a file is done | `status`: `success` \| `skipped` \| `failed` \| `preview`, `out_mb`, `saved_mb`, `saved_pct` |
| `question` | a track selection is due (`-mp4`, `-davinci`, `-split`) | `kind`: `tracks`, `file`, `hint`, `options` (`n`, `label`) |
| `summary` | once, at the very end | `files`, `success`, `skipped`, `failed`, `saved_mb`, `elapsed_sec` |

```json
{"ev":"run","version":"1.18.0","mode":"convert","codec":"h265","encoder":"nvidia","files":1}
{"ev":"stage","index":1,"stage":"analyze"}
{"ev":"progress","index":1,"pct":8.38,"pos_sec":5.03,"eta":"0:06","fps":"-","speed":"9.9x","bitrate":"1k"}
{"ev":"summary","files":1,"success":1,"skipped":0,"failed":0,"saved_mb":71.7,"elapsed_sec":20.9}
```

`status: "preview"` is the honest answer to Ctrl+C: the run was cancelled, but the part that was already encoded is a playable file — and the original is untouched. Without the flag, not a single byte of this is emitted.

**Answering a `question`.** The reply goes back on **stdin** as exactly the line a person would type at the console — `1,3` for single entries, an empty line for all tracks. Nothing else is needed; the numbers in `options[].n` are the ones printed in square brackets on screen.

```json
{"ev":"question","kind":"tracks","file":"C:\\Videos\\Movie.mkv","hint":"Enter = all tracks WITHOUT stereo mix","options":[{"n":1,"label":"Audio  ger  EAC3 6ch"},{"n":2,"label":"  ↳ Stereo mix of [1]  (extra .stereo.m4a)"},{"n":3,"label":"Sub    eng  SUBRIP"}]}
```

In `-json` mode the program then waits **without a time limit** — a front-end shows a dialog, and a running clock would be a trap for anyone taking their time. Two rules follow from that:

- the front-end **must** answer every question; if it closes stdin instead, the safe default (all tracks) applies, exactly as before
- **one answer per question, and only after the announcement** — each prompt reads with its own buffer, so a reply sent ahead of time would be stranded there

At the console nothing changes: `-mp4` still falls back to all tracks after 30 s, so an unattended batch never gets stuck.

**In the tool modes** (`-davinci`, `-split`, `-join`) the same channel is used, with two honest limits. `run` carries no file count — it is not known until the folder has been read. And `summary` carries only `files` and `elapsed_sec`: these modes produce several outputs per source and keep no success/skipped/failed bookkeeping, so those counters stay at zero rather than claiming something. Everything else is the same: one `file` event per source, `progress` while a step runs, `question` when a selection is due.

---

<a id="config-depth"></a>

## ⚙️ Configuration, key by key

Everything lives in `NVENCForge_Config.ini` next to the EXE (auto-created; invalid values are reset to their default in the file individually with a warning, all valid settings left untouched).

**You don't have to touch it at all.** The file is split in two: **PART 1** holds the handful of settings people actually change (`maxResolution`, `autoCQTargetVMAF`, `audioKbpsPerChannel`, `retireMode`, `encoder`), **PART 2** the expert settings, already set to measured values. Every entry explains what it does and which values are allowed. The full list:

CQ quality level, Auto-CQ (on/off, VMAF target, tolerance, plateau savings budget), bitrate caps (H.265 and AV1 separately), resolution cap, NVENC preset/lookahead/B-frames/AQ strength, CAS sharpening, AAC bitrates, auto-shutdown, extra filename characters — plus the [CPU mode](#cpu-depth) block: `encoder` (nvidia/cpu), `cpuPreset`, `cpuAV1Preset`, `cpuTargetCRF`, `cpuAV1TargetCRF` and `cpuThreads`.

One key decides how the encoder **spends its bits** — and it turned out to be worth real money:

| Key | Default | What it does |
|---|---|---|
| `aqStrength` | `2` | How hard the encoder pushes bits towards busy parts of the picture. This used to be hard-wired to `8`, which measured as simply too aggressive. Dropped to `2` (together with one more B-frame), four real sources at a fixed CQ came out **8–28 % smaller at the same quality, with no extra encode time** *(measured 15 Aug 2026)* — 30 fps at 6 Mbit −27.6 %, 50 fps at 12 Mbit −20.9 %, 60 fps at 11 Mbit −12.5 %. Higher values measured monotonically worse *and* bigger. Raise it only if you spot blocky patches in dark, flat areas. |
| `bFrames` | `5` | B-frames are stored as the difference between their neighbours, which saves a lot of space. 5 is the maximum any current NVIDIA card accepts; older cards may take fewer or none, and the startup check finds the highest number yours allows on its own. Not used by AV1. |

Two keys steer **speed** rather than quality:

| Key | Default | What it does |
|---|---|---|
| `gpuDecode` | `true` | Decode on the GPU (NVDEC) instead of the CPU. The picture is bit-identical, so this costs nothing in quality. A decoder error automatically retries the file on the CPU. |
| `gpuDecodeMaxMbit` | `50` | Sources above this bitrate always decode on the CPU. Extreme-bitrate HEVC has been known to crash display drivers, and no fallback can catch that — it has to be avoided beforehand. Typical 4K sources run at 10–30 Mbit/s, so the cautious default costs nothing. Raise it only if your files really are higher. |

One key decides what happens to the **original** once its conversion is done:

| Key | Default | What it does |
|---|---|---|
| `retireMode` | `folder` | `folder` moves the source into an `originals` subfolder next to it — same drive, so it's instant, and nothing is deleted: you check the result and empty that folder yourself. `recyclebin` uses the Windows recycle bin instead (with the checks described below). `-keep` always wins and leaves originals exactly where they are. |

`casStrength` also decides how far the picture gets to stay on the graphics card, because there is no CUDA sharpening filter:

- **`casStrength = 0.4`** (default): downscaled on the GPU, then pulled back into memory once so the CPU can sharpen it. Measured 6 Aug 2026 on 90 s of 4K material: **29 s instead of 35 s**, and the picture ends up slightly *closer* to the source than before, because Lanczos replaced the old bicubic downscale.
- **`casStrength = 0`**: nothing has to come back — decode, downscale and encode all happen on the card. Same material: **13.9 s**, roughly two and a half times faster. The picture is softer without the sharpening pass (VMAF 94.7 against the 4K source, versus 97.3 with it), and Auto-CQ tends to pick a lower CQ, so files come out somewhat larger. Set it back to `0.4` any time.

> Upgrading from an older version? Your existing INI keeps working — unknown keys are ignored and missing ones fall back to their defaults. GPU decoding therefore switches itself on after an update, since `gpuDecode` defaults to `true` — the output stays bit-identical either way. To get the newer blocks written out with their comments, rename the INI and let NVENCForge create a fresh one.

---

<a id="under-the-hood"></a>

## 🔧 Under the hood — the safety nets and clever bits

Most of the work in NVENCForge isn't the encoding itself — FFmpeg does that — it's everything *around* it: keeping your files safe, keeping a batch from ever getting stuck, and giving each file exactly the treatment it needs. Below is the complete list, grouped by what it's for. It's deliberately thorough (this is the nerdy part); every point is real, working behaviour. **Click any section to expand it.**

<details>
<summary><b>🛡️ Your files are safe — no matter what</b></summary>

- **Validate, *then* move aside.** An original is only moved into the `originals` folder *after* the new file has been re-probed and confirmed valid (right codec, no lost audio tracks, plausible duration, sane file size). If validation fails, the original stays exactly where it is.
- **Nothing is deleted — and nothing can silently vanish.** The `originals` folder sits next to the source, on the same drive, so moving a 60 GB file is instant (a rename, not a copy), and *you* decide when it goes. That is deliberately not the recycle bin: Windows empties the bin on its own — Storage Sense, and the `SilentCleanup` task as soon as a drive runs low on space — which can make "safely recycled" originals disappear without a trace. Same-named files from different folders never overwrite each other (`clip (2).mkv`), and if a move fails for any reason the original simply stays put.
- **Recycle bin still available, and now honest about it.** Prefer the bin? Set `retireMode=recyclebin`. NVENCForge then checks *beforehand* whether that drive's bin can actually accept the file (drive type, per-volume setting, and the bin's size limit — Windows would otherwise answer its own "too big for the recycle bin?" prompt with *yes* and destroy the file silently), and *afterwards* whether the file really arrived. If the bin can't take it, the original is kept and you're told why.
- **Never overwrite anything.** If an output name is already taken, NVENCForge writes an automatic numbered name (`Movie.2`, `Movie.3`, …). Nothing you already have is ever clobbered.
- **A re-encode is never bigger than the source.** If a conversion somehow comes out larger (it happens on already-lean files), the result is thrown away and the file is losslessly repackaged instead. You never trade quality *and* gain size.
- **Crash-safe output.** Abort mid-encode, close the window, or lose power — you're left with a playable `.preview.mkv`, never a corrupt zero-byte file. FFmpeg is asked to *finish cleanly* (a graceful "q"), not killed mid-frame.
- **Video-only fallback keeps the original.** If the only way to salvage a file is to drop its audio, the source — the only copy of that audio — is deliberately **not** moved aside at all.
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
- **Encoder tuned for quality-per-bit.** VBR + CQ, `tune hq`, spatial **and** temporal adaptive quantisation, multi-pass, look-ahead, B-frames with a pyramid reference (as many as your card allows — measured at startup), constant frame rate, and a keyframe interval sized to ~4 seconds of video — the whole reason the quality hit stays small at hardware-encode speed.

</details>

<details>
<summary><b>🚦 It won't fall over — robustness during a batch</b></summary>

- **A GPU check that matches reality — and finds your card's real limits.** At startup NVENCForge does a dummy encode with the *exact* flags the real encode uses — not a lighter test — so a card that would fail later is caught now. If your card takes them, that is the entire check: one probe, nothing else runs. If it refuses, NVENCForge asks the card what it *can* do rather than switching everything off at once: the B-frame count is counted down until one is accepted, `b_ref_mode` and Temporal AQ are probed as capabilities of their own (a card missing one keeps the other), and a card short on memory is offered a smaller look-ahead window instead of being declared unusable. Every fallback is reported on screen together with the config line that makes it permanent. There is deliberately **no built-in table of GPU models** — such a list would be guesswork for every card nobody could test, and it would age with each new generation. AV1 gets its own separate probe.
- **GPU decoding, with a deliberate ceiling.** Decoding runs on the graphics card (NVDEC) — bit-identical to the CPU path, just faster. Above `gpuDecodeMaxMbit` (50 Mbit/s by default) it falls back to the CPU on purpose: hardware-decoding extreme-bitrate HEVC can crash the display driver (a TDR reset), and nothing can catch that afterwards. A decoder error retries the file on the CPU by itself.
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

- **Live progress you can trust.** A per-file *and* an overall batch bar, with ETA, encode speed, fps, bitrate, frame count — and a continuously-smoothed *projected* output size that already tells you "≈ −60 %" long before the file is done. The batch bar is weighted by file size rather than file count (a two-hour film is not one "file" of work like an eight-minute episode) and shows the time left for the **whole run** — the Auto-CQ analysis included, because the estimate is taken from the wall clock. The cursor is hidden and line-wrap disabled during the render so nothing smears.
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
- **Send-to drag & drop.** Wire it into the Windows "Send to" menu and never touch a command line again.
- **Auto-shutdown** for long overnight batches (`-shutdown`, with a 30-second cancel window).

</details>

---

<a id="building"></a>

## 🔨 Building from source

```
cd sourcecode
build.bat
```

That's it. `build.bat` packs the embedded source archive and compiles `NVENCForge.exe` (Go 1.21+). And remember: every released EXE extracts its own sources on first run.

---

**[← Back to the README](README.md)**
