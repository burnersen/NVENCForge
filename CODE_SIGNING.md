# Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io),
certificate by [SignPath Foundation](https://signpath.org).

> **Status:** NVENCForge has applied to the SignPath Foundation. Until the
> signing is active, `NVENCForge.exe` is not signed. Every release says in
> its text whether its exe is signed.

## What is signed

Only `NVENCForge.exe`, built from the source code in this repository by the
[release workflow](.github/workflows/release.yml) on GitHub-hosted runners.
Nothing that was built elsewhere is signed with this certificate. Every
signing request is approved by hand, release by release.

FFmpeg is not part of NVENCForge and is never signed by this project: on
first run NVENCForge downloads an official build of the
[BtbN FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) project, or uses
the `ffmpeg.exe` you put next to it.

The file properties of the signed exe name the product `NVENCForge` and the
version of the release; SignPath refuses to sign a file that does not match.

## Team roles

| Role | Who |
| ---- | --- |
| Committers and reviewers | [burnersen](https://github.com/burnersen) |
| Approvers | [burnersen](https://github.com/burnersen) |

Changes from anybody else (pull requests) are reviewed by a committer before
they are merged. All team members use multi-factor authentication for GitHub
and for SignPath.

## Privacy policy

This program will not transfer any information to other networked systems
unless specifically requested by the user or the person installing or
operating it.

What NVENCForge does on the network, and only for that:

- **FFmpeg download:** only if there is no `ffmpeg.exe` / `ffprobe.exe` next
  to the exe, NVENCForge asks the GitHub API for the newest release of
  [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) and downloads
  it from GitHub. Nothing else is sent.

Like any network request, this reveals your IP address to GitHub. Your
videos are converted on your own PC and never leave it. There is no
telemetry, no update check, no account and no advertising.

## Installing and uninstalling

There is no installer: NVENCForge is one exe. It keeps its settings
(`NVENCForge_Config.ini`), the downloaded FFmpeg and its log next to that exe
and writes nothing to the registry. To uninstall, delete the folder. If you
added NVENCForge to the "Send to" menu, also delete that shortcut
(`Win+R`, `shell:sendto`).

## Verifying a download

- **Signature:** right-click `NVENCForge.exe` > *Properties* > *Digital
  Signatures*. A signed release shows *SignPath Foundation*.
- **Build it yourself:** see [TECHNICAL.md](TECHNICAL.md#building).

## Reporting a problem

If you think a signed file of this project does something it should not,
please open an [issue](https://github.com/burnersen/NVENCForge/issues).
Signed files that break these rules can also be reported to the
[SignPath Foundation](https://signpath.org).
