# Code signing with SignPath: prepared, off until switched on

NVENCForge has applied to the [SignPath Foundation](https://signpath.org) for free
code signing of its Windows files. Everything for it is prepared here and in
[`release.yml`](../.github/workflows/release.yml), but **it stays off** until it is
switched on below. Until then every release is built and published exactly as before,
unsigned, and its release text says so.

| File | What it is |
| ---- | ---------- |
| [`../CODE_SIGNING.md`](../CODE_SIGNING.md) | The public code signing policy (required by SignPath Foundation): roles, what is signed, privacy |
| [`artifact-configuration.xml`](artifact-configuration.xml) | The SignPath artifact configuration (the project's default): signs NVENCForge.exe only |
| [`check-files.ps1`](check-files.ps1) | Run by every release build: checks product name and version of every file to be signed (SignPath enforces both), and with `-Signed` that the signatures are valid |

## How the release workflow decides

| Repository settings | What a release does |
| ------------------- | ------------------- |
| Neither `SIGNPATH_ORGANIZATION_ID` nor `SIGNPATH_API_TOKEN` set | Builds and publishes unsigned, as before |
| Both set | Uploads the files, sends a signing request to SignPath, **waits up to one hour for the approval**, downloads the signed files, checks their signatures and publishes them |
| Only one of them set | Stops with an error, so a half finished setup never ships unsigned files by surprise |

**Dry run:** *Actions > Release > Run workflow* with "dry run" ticked builds and checks
everything, keeps the files on the run page as a workflow artifact, never signs and
publishes nothing. It also runs by itself for every pull request that changes the
release workflow or this folder.

## Switching it on (after SignPath Foundation said yes)

1. **Two-factor authentication** on GitHub and on SignPath for everybody in the team
   (a condition of SignPath Foundation).
2. **In SignPath** (the Foundation sets up most of it with you):
   - the project, slug `NVENCForge` (another slug: set the variable `SIGNPATH_PROJECT_SLUG`),
   - the trusted build system *GitHub.com* linked to the project, and the SignPath
     GitHub App installed on this repository (SignPath shows the link),
   - the artifact configuration from [`artifact-configuration.xml`](artifact-configuration.xml), set as the project's default,
   - the signing policy `release-signing` with manual approval by the approver
     (another slug: set `SIGNPATH_SIGNING_POLICY_SLUG`),
   - a CI user with the *Submitter* role on that policy, and an API token for it.
3. **On GitHub:** *Settings > Secrets and variables > Actions*
   - secret `SIGNPATH_API_TOKEN`: the API token of the CI user,
   - variable `SIGNPATH_ORGANIZATION_ID`: the organization ID from SignPath.
4. **Recommended:** a ruleset that protects `main` and the `v*` tags from force pushes
   and deletion (*Settings > Rules > Rulesets*). SignPath can require this in its
   policies.
5. **Update the texts:** the status line at the top of
   [`CODE_SIGNING.md`](../CODE_SIGNING.md) and the SmartScreen note in the README.
6. **Release as usual.** Approve the signing request in SignPath within one hour (the
   link is in the run log and in the e-mail from SignPath), then the release is published
   with signed files.

| Variable / secret | Required | Default |
| ----------------- | -------- | ------- |
| `SIGNPATH_API_TOKEN` (secret) | yes | |
| `SIGNPATH_ORGANIZATION_ID` | yes | |
| `SIGNPATH_PROJECT_SLUG` | no | `NVENCForge` |
| `SIGNPATH_SIGNING_POLICY_SLUG` | no | `release-signing` |
| `SIGNPATH_ARTIFACT_CONFIGURATION_SLUG` | no | the project's default artifact configuration |

## Taking it out again

Never setting the secret and the variable is enough: nothing is signed then. To remove
it completely: delete this folder and `CODE_SIGNING.md`, remove the steps marked
`code signing` in `release.yml` (the file check step uses `check-files.ps1` from this
folder) and the links to `CODE_SIGNING.md` in the README.
