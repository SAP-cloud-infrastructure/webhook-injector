#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
# SPDX-License-Identifier: Apache-2.0
#
# Mirror the webhook-injector container image from GHCR (built by CI) into the
# Keppel registry, preserving digests and multi-arch manifests.
#
# The image is NOT rebuilt — it is copied registry-to-registry so the artifact
# in Keppel is bit-for-bit identical to what the "Container Registry GHCR"
# workflow produced on the last push to main.
#
# Usage:
#   scripts/mirror-image-to-keppel.sh [--all] [TAG ...]
#
# With no arguments it mirrors the default GHCR tag set: latest, edge, and the
# long git-sha tag for the current HEAD (sha-<full-sha>) if it exists.
# With --all it lists every tag present in the GHCR repo and mirrors all of
# them (requires the crane backend). Otherwise it mirrors exactly the tags you
# pass, e.g.:
#   scripts/mirror-image-to-keppel.sh v0.1.0 latest
#   scripts/mirror-image-to-keppel.sh --all
#
# Auth (Keppel — OpenStack credentials):
#   Log in once before running, or set KEPPEL_USERNAME / KEPPEL_PASSWORD and the
#   script logs in for you.
#     docker login keppel.eu-de-2.cloud.sap
#     Username: <user>@<domain>/<project>@<project-domain>   e.g. D065300@ccadmin/cloud_admin@ccadmin
#     Password: <your OpenStack password>
#
# Auth (GHCR — source pull):
#   The GHCR package is PRIVATE, so pulling needs a token with the
#   read:packages scope. Export it before running:
#     export GHCR_TOKEN=<PAT-or-CI-GITHUB_TOKEN-with-read:packages>
#     export GHCR_USER=<github-login>   # optional; defaults to $GITHUB_ACTOR/$USER
#   A plain `gh auth token` will NOT work unless it carries read:packages.
set -euo pipefail

# GHCR repo paths are lowercase; docker/metadata-action lowercases
# github.repository, so the CI-produced image lives at the lowercase path.
SRC_IMAGE="ghcr.io/sap-cloud-infrastructure/webhook-injector"
KEPPEL_REGISTRY="keppel.eu-de-2.cloud.sap"
KEPPEL_ACCOUNT="i-cant-believe-its-not-cloud-infrastructure-dev"
DST_IMAGE="${KEPPEL_REGISTRY}/${KEPPEL_ACCOUNT}/webhook-injector"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# --- Pick a copy backend: crane (preferred) or docker (fallback) -------------
COPY_BACKEND=""
if command -v crane >/dev/null 2>&1; then
  COPY_BACKEND="crane"
elif command -v docker >/dev/null 2>&1; then
  COPY_BACKEND="docker"
else
  die "neither 'crane' nor 'docker' found on PATH; install one to mirror images"
fi
log "Using ${COPY_BACKEND} backend"

# --- Log in to GHCR if credentials are provided (the package is private) -----
# GHCR needs a token with the read:packages scope to pull a private package.
# Provide it via GHCR_TOKEN (a classic PAT or the CI GITHUB_TOKEN); GHCR_USER
# defaults to your GitHub login.
if [[ -n "${GHCR_TOKEN:-}" ]]; then
  ghcr_user="${GHCR_USER:-${GITHUB_ACTOR:-$USER}}"
  log "Logging in to ghcr.io as ${ghcr_user}"
  case "${COPY_BACKEND}" in
    crane)  printf '%s' "${GHCR_TOKEN}" | crane auth login ghcr.io -u "${ghcr_user}" --password-stdin ;;
    docker) printf '%s' "${GHCR_TOKEN}" | docker login ghcr.io -u "${ghcr_user}" --password-stdin ;;
  esac
fi

# --- Log in to Keppel if credentials are provided via env --------------------
if [[ -n "${KEPPEL_USERNAME:-}" && -n "${KEPPEL_PASSWORD:-}" ]]; then
  log "Logging in to ${KEPPEL_REGISTRY} as ${KEPPEL_USERNAME}"
  case "${COPY_BACKEND}" in
    crane)  printf '%s' "${KEPPEL_PASSWORD}" | crane auth login "${KEPPEL_REGISTRY}" -u "${KEPPEL_USERNAME}" --password-stdin ;;
    docker) printf '%s' "${KEPPEL_PASSWORD}" | docker login "${KEPPEL_REGISTRY}" -u "${KEPPEL_USERNAME}" --password-stdin ;;
  esac
else
  warn "KEPPEL_USERNAME/KEPPEL_PASSWORD not set — assuming you already ran 'docker login ${KEPPEL_REGISTRY}'"
fi

# --- Determine which tags to mirror ------------------------------------------
tags=()
if [[ "${1:-}" == "--all" ]]; then
  [[ "${COPY_BACKEND}" == "crane" ]] || die "--all requires the crane backend (docker cannot list remote tags); install crane"
  log "Listing all tags in ${SRC_IMAGE}"
  while IFS= read -r t; do
    [[ -n "${t}" ]] && tags+=("${t}")
  done < <(crane ls "${SRC_IMAGE}")
  [[ ${#tags[@]} -gt 0 ]] || die "no tags found in ${SRC_IMAGE} (auth problem? set GHCR_TOKEN with read:packages)"
elif [[ $# -gt 0 ]]; then
  tags=("$@")
else
  log "No tags given; using default GHCR tag set"
  tags=("latest" "edge")
  if sha="$(git rev-parse HEAD 2>/dev/null)"; then
    tags+=("sha-${sha}")   # matches metadata-action type=sha,format=long
  fi
fi
log "Tags to mirror: ${tags[*]}"

# --- Probe a source tag -------------------------------------------------------
# Returns: "present" | "absent" | "denied". A denied result means an auth
# problem (private GHCR package + missing read:packages), NOT a missing tag —
# those must fail loudly rather than be silently skipped.
src_probe() {
  local ref="$1" out rc
  case "${COPY_BACKEND}" in
    crane)  out="$(crane manifest "${ref}" 2>&1 >/dev/null)"; rc=$? ;;
    docker) out="$(docker manifest inspect "${ref}" 2>&1 >/dev/null)"; rc=$? ;;
  esac
  if [[ ${rc} -eq 0 ]]; then
    echo present; return
  fi
  if grep -qiE 'denied|unauthorized|forbidden|authentication' <<<"${out}"; then
    echo denied
  else
    echo absent
  fi
}

copy_tag() {
  local tag="$1"
  local src="${SRC_IMAGE}:${tag}"
  local dst="${DST_IMAGE}:${tag}"

  case "$(src_probe "${src}")" in
    denied)
      die "access to ${src} DENIED — the GHCR package is private. Set GHCR_TOKEN to a token with the read:packages scope (see script header)"
      ;;
    absent)
      warn "source ${src} does not exist — skipping"
      return 1
      ;;
  esac

  log "Mirroring ${src} -> ${dst}"
  case "${COPY_BACKEND}" in
    crane)
      crane copy "${src}" "${dst}"
      ;;
    docker)
      docker pull "${src}"
      docker tag "${src}" "${dst}"
      docker push "${dst}"
      ;;
  esac
}

failed=0
mirrored=0
for tag in "${tags[@]}"; do
  if copy_tag "${tag}"; then
    mirrored=$((mirrored + 1))
  else
    failed=$((failed + 1))
  fi
done

log "Done: ${mirrored} tag(s) mirrored, ${failed} skipped/failed"
[[ "${mirrored}" -gt 0 ]] || die "no tags were mirrored"
[[ "${failed}" -eq 0 ]] || warn "some tags were skipped/failed (see above)"
