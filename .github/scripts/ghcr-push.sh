#!/usr/bin/env bash
# Publish a locally built image to GHCR without ever moving a tag that is
# already there. Used by the `image-*` jobs in .github/workflows/ci.yml.
#
#   ghcr-push.sh probe <remote-ref>                    print present|absent
#   ghcr-push.sh push  <local-image> <remote-ref> reuse   absent: push; present: keep it
#   ghcr-push.sh push  <local-image> <remote-ref> refuse  absent: push; present: FAIL
#
# 🔴 A PUBLISHED TAG IS NEVER OVERWRITTEN, AND THAT IS THE WHOLE POINT OF THIS
# SCRIPT. A docker build is not byte-reproducible, so re-running a `main` run
# (`gh run rerun`) would otherwise re-point `<image>:<sha>` at a different
# digest than the one a deploy may already have pinned — the same hazard
# devrc's `muster-ops release` guards against for the Harbor tags. `reuse` is for
# tags that name a commit (the same commit gives the same source, so the
# existing image stands); `refuse` is for a release version, where a second
# image under one version is a mistake to stop on, not to paper over.
#
# 🔴 ABSENCE IS CLASSIFIED, NEVER INFERRED FROM A FAILURE. `probe` reports
# `absent` only on the registry's own not-found answers; any other failure (auth,
# network, rate limit) exits 2 with the registry's message. Otherwise a broken
# login would read as "nothing published yet" and the push would decide what a
# tag means.
#
# Exit codes: 0 done / probe answered; 1 refused (tag taken under `refuse`, or a
# push failed); 2 harness error (bad arguments, a probe it could not classify).
set -euo pipefail

die() { echo "ghcr-push: $1" >&2; exit "${2:-1}"; }

# Before any probe: a missing docker prints "command not found", which the
# not-found classification below would otherwise read as "absent".
command -v docker >/dev/null 2>&1 || die "docker is not on PATH" 2

probe() {  # present | absent on stdout; exit 2 on anything else
  local ref="$1" out
  if out="$(docker manifest inspect "$ref" 2>&1)"; then
    echo present; return 0
  fi
  # GHCR's answers for a missing tag (`manifest unknown`) and for a package that
  # has never been published (measured anonymously: also `manifest unknown`).
  if grep -qiE 'manifest unknown|name unknown|no such manifest' <<<"$out"; then
    echo absent; return 0
  fi
  echo "ghcr-push: could not tell whether $ref exists: $out" >&2
  exit 2
}

summary() { [ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY" || true; }

digest_of() {  # the digest the registry serves for a ref
  docker buildx imagetools inspect "$1" --format '{{json .Manifest}}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["digest"])'
}

cmd="${1:-}"
case "$cmd" in
  probe)
    [ $# -eq 2 ] || die "usage: $0 probe <remote-ref>" 2
    probe "$2"
    ;;
  push)
    [ $# -eq 4 ] || die "usage: $0 push <local-image> <remote-ref> reuse|refuse" 2
    local_img="$2"; ref="$3"; mode="$4"
    case "$mode" in reuse|refuse) ;; *) die "mode must be reuse or refuse, got '$mode'" 2 ;; esac
    docker image inspect "$local_img" >/dev/null 2>&1 || die "no local image $local_img" 2
    state="$(probe "$ref")"
    if [ "$state" = present ]; then
      [ "$mode" = refuse ] && die "$ref is already published; a release version is never re-pushed — cut the next version"
      d="$(digest_of "$ref")"
      echo "ghcr-push: $ref already published at $d — kept, not overwritten"
      summary "- \`$ref@$d\` (already published, kept)"
      exit 0
    fi
    docker tag "$local_img" "$ref"
    docker push "$ref"
    d="$(digest_of "$ref")"
    echo "ghcr-push: pushed $ref@$d"
    summary "- \`$ref@$d\` (pushed)"
    ;;
  *)
    die "usage: $0 probe <remote-ref> | push <local-image> <remote-ref> reuse|refuse" 2
    ;;
esac
