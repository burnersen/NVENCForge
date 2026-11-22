#!/usr/bin/env bash
# Security guard: finds published security flaws in Go itself and in the Go
# modules of this repo, fixes them, tests, and publishes a new patch release.
# Runs from .github/workflows/guard.yml; everything repo specific comes from
# the GUARD_* variables there. Does nothing at all when there is nothing to do.
#
# Stops without releasing (and opens an issue, so GitHub sends a mail) when:
# a flaw stays after the update, the tests fail, or a step of its own fails.
set -Eeuo pipefail

: "${GUARD_NAME:?}" "${GUARD_MODULES:?}" "${GUARD_GO:?}" "${GUARD_TEST:?}" "${GUARD_NOTES:?}" "${GUARD_RELEASE:?}"
GUARD_LANG="${GUARD_LANG:-en}"
GUARD_DRY="${GUARD_DRY:-false}"
GO_FILE=".github/guard/go-version"
ISSUE_TITLE="Security guard needs a look"
WORK="${RUNNER_TEMP:-/tmp}/guard"
mkdir -p "$WORK"

log() { echo "guard: $*"; }

fail() {
  log "STOP: $1"
  if [ "$GUARD_DRY" = true ]; then exit 1; fi
  local body
  body=$(printf '%s\n\n%s\n\nRun: %s/%s/actions/runs/%s\n' \
    "The security guard stopped and published nothing." "$1" \
    "$GITHUB_SERVER_URL" "$GITHUB_REPOSITORY" "$GITHUB_RUN_ID")
  local open
  open=$(gh issue list --state open --search "\"$ISSUE_TITLE\" in:title" --json number -q '.[0].number' || true)
  if [ -n "$open" ]; then gh issue comment "$open" --body "$body"; else gh issue create --title "$ISSUE_TITLE" --body "$body"; fi
  exit 1
}
trap 'fail "Unexpected error in line $LINENO of guard.sh."' ERR

# scan <outfile>: govulncheck JSON for every module, Windows and Linux view.
scan() {
  : > "$1"
  local ok=0 d
  for d in $GUARD_MODULES; do
    for os in windows linux; do
      if (cd "$d" && GOOS=$os govulncheck -format json ./... >> "$1" 2> "$WORK/scan-err.txt"); then ok=1
      else
        # Exit code 3 only means "found something": the JSON is still complete.
        [ $? -eq 3 ] && ok=1 || true
      fi
    done
  done
  [ $ok = 1 ] || fail "govulncheck could not scan the code: $(head -c 600 "$WORK/scan-err.txt")"
}

# fixable <json>: "<module> <osv>" for every finding that has a fix.
fixable() {
  jq -r 'select(.finding and .finding.fixed_version) | "\(.finding.trace[0].module) \(.finding.osv)"' "$1" | sort -u
}

# --- 1. scan the code as it ships ----------------------------------------
gh api -X PUT "repos/$GITHUB_REPOSITORY/actions/workflows/guard.yml/enable" >/dev/null 2>&1 || true
log "shipped with $(go version)"
go install golang.org/x/vuln/cmd/govulncheck@latest
scan "$WORK/before.json"
fixable "$WORK/before.json" > "$WORK/found.txt"
if [ ! -s "$WORK/found.txt" ] && [ "${GUARD_SIMULATE:-false}" = true ] && [ "$GUARD_DRY" = true ]; then
  log "simulating a flaw in Go itself (dry run only)"
  echo "stdlib GO-0000-0000" > "$WORK/found.txt"
fi
if [ ! -s "$WORK/found.txt" ]; then
  log "no known security flaw, nothing to do"
  exit 0
fi
log "found:"; cat "$WORK/found.txt"

std_ids=$(awk '$1=="stdlib"{print $2}' "$WORK/found.txt" | sort -u | tr '\n' ' ')
mods=$(awk '$1!="stdlib"{print $1}' "$WORK/found.txt" | sort -u | tr '\n' ' ')

# --- 2. switch to the newest Go and update the modules ---------------------
sg="$GUARD_STABLE_GOROOT"; command -v cygpath >/dev/null && sg=$(cygpath -u "$sg")
export PATH="$sg/bin:$PATH"
newgo=$(go env GOVERSION); newgo="${newgo#go}"
log "updating with Go $newgo"
if [ -n "$std_ids" ]; then
  if [ "$GUARD_GO" = gomod ]; then
    for d in $GUARD_MODULES; do sed -i -E "s/^go [0-9][0-9.]*\$/go $newgo/" "$d/go.mod"; done
  else
    echo "$newgo" > "$GO_FILE"
  fi
fi
updated=""
for d in $GUARD_MODULES; do
  for m in $mods; do
    if grep -qE "^\s*(require )?$m v" "$d/go.mod"; then
      (cd "$d" && go get "$m@latest")
    fi
  done
  (cd "$d" && go mod tidy)
done
for m in $mods; do
  v=$(cd "$(echo "$GUARD_MODULES" | awk '{print $1}')" && go list -m -f '{{.Version}}' "$m" 2>/dev/null || true)
  [ -z "$v" ] && for d in $GUARD_MODULES; do v=$(cd "$d" && go list -m -f '{{.Version}}' "$m" 2>/dev/null || true); [ -n "$v" ] && break; done
  updated="$updated$m ${v#v}"$'\n'
done

# --- 3. check again and test ---------------------------------------------
go install golang.org/x/vuln/cmd/govulncheck@latest
scan "$WORK/after.json"
fixable "$WORK/after.json" > "$WORK/left.txt"
if [ -s "$WORK/left.txt" ]; then
  fail "After the update these flaws are still there: $(tr '\n' ';' < "$WORK/left.txt")"
fi
log "testing: $GUARD_TEST"
bash -c "$GUARD_TEST" > "$WORK/test.txt" 2>&1 || fail "The tests failed after the update. Last lines:
\`\`\`
$(tail -n 40 "$WORK/test.txt")
\`\`\`"

# --- 4. new version ------------------------------------------------------
prev=$(gh release view --json tagName -q .tagName); prev="${prev#v}"
IFS=. read -r ma mi pa <<< "${prev%%-*}"
ver="$ma.$mi.$((pa + 1))"
log "version $prev -> $ver"
while IFS= read -r spot; do
  [ -z "$spot" ] && continue
  f="${spot%%|*}"; match="${spot#*|}"
  grep -F "$match" "$f" | grep -qF "$prev" || fail "Version $prev not found in $f (line with \"$match\"). The guard's settings need an update."
  sed -i "/$(printf '%s' "$match" | sed 's/[\/&.[\*^$]/\\&/g')/ s/$(printf '%s' "$prev" | sed 's/\./\\./g')/$ver/" "$f"
done <<< "${GUARD_VERSION_SPOTS:-}"

link() { local out="" id; for id in $1; do out="$out${out:+, }[$id](https://pkg.go.dev/vuln/$id)"; done; echo "$out"; }
mod_ids() { awk -v m="$1" '$1==m{print $2}' "$WORK/found.txt" | sort -u | tr '\n' ' '; }

items=""
if [ "$GUARD_LANG" = de ]; then
  title="## $GUARD_NAME $ver: Sicherheitsupdate"
  intro="$GUARD_NAME funktioniert genau wie in $prev. Diese Version ist automatisch entstanden, weil eine Sicherheitslücke in Go oder in einer verwendeten Bibliothek bekannt wurde."
  [ -n "$std_ids" ] && items="$items- **Gebaut mit Go $newgo**, das behebt: $(link "$std_ids")."$'\n'
  while read -r m v; do [ -z "$m" ] && continue
    items="$items- **Bibliothek aktualisiert:** \`$m\` auf $v, das behebt: $(link "$(mod_ids "$m")")."$'\n'
  done <<< "$updated"
else
  title="## $GUARD_NAME $ver: security update"
  intro="$GUARD_NAME works exactly as in $prev. This version was made automatically because a security flaw was published in Go or in a library it uses."
  [ -n "$std_ids" ] && items="$items- **Built with Go $newgo**, which fixes $(link "$std_ids")."$'\n'
  while read -r m v; do [ -z "$m" ] && continue
    items="$items- **Updated library:** \`$m\` $v, which fixes $(link "$(mod_ids "$m")")."$'\n'
  done <<< "$updated"
fi
notes="$title"$'\n\n'"$intro"$'\n\n'"$items"
[ -n "${GUARD_FOOTER:-}" ] && notes="$notes"$'\n'"$GUARD_FOOTER"$'\n'

case "$GUARD_NOTES" in
  changelog:*)
    f="${GUARD_NOTES#changelog:}"
    entry="## $ver ($(date -u +%Y-%m-%d))"$'\n\n'"$intro"$'\n\n'"$items"
    "$(command -v python3 || command -v python)" - "$f" "$entry" <<'PY'
import sys
p, entry = sys.argv[1], sys.argv[2]
s = open(p, encoding="utf-8").read()
head = "# Changelog\n\n"
s = s.replace(head, head + entry + "\n", 1) if s.startswith(head) else entry + "\n" + s
open(p, "w", encoding="utf-8").write(s)
PY
    ;;
  tag) printf '%s' "$notes" > "$WORK/tag-message.md" ;;
  *) f="${GUARD_NOTES//\{V\}/$ver}"; mkdir -p "$(dirname "$f")"; printf '%s' "$notes" > "$f" ;;
esac

git status --short
if [ "$GUARD_DRY" = true ]; then
  log "dry run: this would be released as $ver"
  echo "$notes"
  git --no-pager diff
  exit 0
fi

# --- 5. commit, release, watch ------------------------------------------
git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git add -A
git commit -q -m "Security update $ver (automatic)"
git push origin HEAD:main || fail "Pushing to main failed (was main changed at the same time?)."

if [ "$GUARD_NOTES" = tag ]; then
  git tag -a "v$ver" -F "$WORK/tag-message.md"
  git push origin "v$ver"
fi

args=()
IFS=',' read -ra pairs <<< "$GUARD_RELEASE"
for p in "${pairs[@]}"; do
  [ "$p" = none ] && continue
  args+=(-f "${p//\{V\}/$ver}")
done
before=$(gh run list --workflow release.yml --event workflow_dispatch -L 1 --json databaseId -q '.[0].databaseId' || true)
gh workflow run release.yml --ref main "${args[@]}"
for _ in $(seq 1 30); do
  sleep 10
  run=$(gh run list --workflow release.yml --event workflow_dispatch -L 1 --json databaseId -q '.[0].databaseId' || true)
  [ -n "$run" ] && [ "$run" != "$before" ] && break
done
[ -n "${run:-}" ] && [ "$run" != "$before" ] || fail "The release workflow did not start."
gh run watch "$run" --exit-status --interval 60 > /dev/null || fail "The release of $ver failed: $GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$run"
log "released $ver"
