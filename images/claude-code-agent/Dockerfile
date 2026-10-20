# claude-code-agent: an interactive Claude Code session in tmux, driven by ccd.
#
#   docker build -f images/claude-code-agent/Dockerfile -t claude-code-agent .
#   docker run --rm --tmpfs /data:rw,exec,mode=2777,uid=0,gid=1000 \
#     -e CC_SMOKE_VOLUME=fresh claude-code-agent cc-smoke
#
# (a tmpfs, not a named volume: docker copies an image's /data into an empty
# named volume, uid 1000 ownership included, which would hide what a fresh
# Kubernetes volume looks like)
#
# The build context is the REPOSITORY ROOT: ccd is built from this module
# (cmd/ccd), which is the point — its /v1/responses wire is tested against
# muster's own client in the same module, so the two cannot drift.

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ccd ./cmd/ccd

# 🔴 node:22-slim + the npm package, NOT the curl-pipe native installer. The npm
# package ships the same native binary, the version is an exact registry pin
# resolved at build time (the native installer fetches from a URL at build time
# with nothing in this file to pin), and this is the combination the scoping
# experiments ran. Debian (not alpine) because the shipped binary is glibc-linked.
FROM node:22-bookworm-slim

# curl is for muster's connector proxy: an agent calls a platform's HTTP API as
# `curl -H "Authorization: Bearer $MUSTER_HOOK_TOKEN" "$MUSTER_CONNECT_URL/connect/<connector>/<host>/<path>"`,
# and muster replaces it with the credential (see cmd/ccd/connect.go). git goes
# through the proxy by routing `ccd serve` keeps in step with the agent's grants
# (`ccd connect-git` applies it at once). `gh` is NOT installed: it only speaks HTTPS to a
# fixed host, so it cannot be pointed at the plain-HTTP in-cluster proxy.
#
# 🔴 PINNED. A floating CLI is a different program on every rebuild, and ccd parses
# that program's transcript and hook payloads. Bump deliberately, re-record the
# fixtures in cmd/ccd/testdata, and re-run the operator live check.
ARG CLAUDE_CODE_VERSION=2.1.296
ENV CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}

RUN apt-get update \
 && apt-get install -y --no-install-recommends tmux git ca-certificates procps curl \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g --no-audit --no-fund "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm cache clean --force \
 && claude --version | grep -F "${CLAUDE_CODE_VERSION}"

COPY --from=build /ccd /usr/local/bin/ccd
COPY images/claude-code-agent/settings.json /etc/ccd/settings.json
COPY --chmod=0755 images/claude-code-agent/entrypoint.sh /usr/local/bin/cc-entrypoint
COPY --chmod=0755 images/claude-code-agent/smoke.sh /usr/local/bin/cc-smoke

# /data is the persistent volume's mount point: the CLI's config (credentials from
# /login, settings, and the session transcripts `--resume <id>` resumes) and the
# workspace both live under it. Kept as two sibling directories so the config
# never sits inside a repository a session might commit.
#
# DISABLE_AUTOUPDATER: the version is pinned above, and the global npm prefix is
# not writable by uid 1000 anyway.
ENV CLAUDE_CONFIG_DIR=/data/claude \
    CCD_WORKSPACE=/data/workspace \
    DISABLE_AUTOUPDATER=1 \
    HOME=/home/node
RUN install -d -o 1000 -g 1000 -m 0700 /data /data/claude /data/workspace

# uid 1000 is the base image's `node` user.
USER 1000:1000
# 🔴 NOT /data/workspace. A container runtime creates a missing WORKDIR as ROOT,
# AFTER mounting volumes: on a fresh volume at /data that made /data/workspace
# root-owned, mode 0755 (fsGroup sets only its group), and every agent with a
# repository crash-looped on `ccd seed: mkdir /data/workspace/<repo>: permission
# denied` (0.49.0). `ccd seed` creates the workspace as uid 1000 and replaces
# such a root-owned EMPTY directory on volumes those images already started on
# (cmd/ccd/seed.go ensureWritableDir); the entrypoint then cd's into
# CCD_WORKSPACE, and ccd starts the CLI there, so the CLI's working directory —
# the key its transcripts are filed under — is unchanged.
WORKDIR /
EXPOSE 18789
ENTRYPOINT ["/usr/local/bin/cc-entrypoint"]
