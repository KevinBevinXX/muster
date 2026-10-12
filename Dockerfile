# syntax=docker/dockerfile:1

# muster's server image.
#
# 🔴 THREE STAGES, AND THE FIRST ONE EXISTS ENTIRELY TO MAKE A SILENT FAILURE
# LOUD. web/static/app.css is `go:embed`ed, so the Go build cannot start without
# it — and Tailwind CANNOT FAIL THE WAY YOU NEED IT TO. A content entry that
# matches nothing is not an error and not a non-zero exit: Tailwind emits a
# smaller, perfectly valid stylesheet and exits 0. Measured on this tree with a
# glob deliberately pointed at a directory that does not exist: 5,594 valid bytes
# instead of the real ~38,000, exit code 0, no warning. The page then renders
# unstyled wherever that stylesheet is SERVED — a container, typically — while
# looking correct on the machine the build ran on. The first report is "it looks
# broken in prod".

# --- Stage 1: build and VERIFY the Tailwind stylesheet ----------------------
FROM node:20-alpine AS css
WORKDIR /app
# ⚠ bash IS NOT OPTIONAL. The Makefile sets `SHELL := /usr/bin/env bash` and
# css-check's recipe is a bash script; alpine ships busybox ash and no bash, so
# without this the stage fails with a shell error that says nothing about CSS.
RUN apk add --no-cache make bash
COPY package.json ./
RUN npm install --no-audit --no-fund

# 🔴 EVERY PATH IN tailwind.config.js's `content` ARRAY MUST BE COPIED INTO THIS
# STAGE, AND A MISSING COPY IS EXACTLY THE SILENT CASE ABOVE. The globs scan
# internal/ui/**/*.go (where essentially every class is written) and
# internal/api/login.go (the one document rendered outside internal/ui — the
# sign-in page, which nothing else would notice losing).
COPY tailwind.config.js Makefile ./
COPY web/ ./web/
COPY internal/ui/ ./internal/ui/
COPY internal/api/login.go ./internal/api/login.go
# The gotrue-mode sign-in, claim and account pages (accounts PR 4).
COPY internal/api/login_gotrue.go ./internal/api/login_gotrue.go
COPY internal/api/api_tokens.go ./internal/api/api_tokens.go

# 🔴 `make css-check`, NOT A HAND-ROLLED `test -s` PLUS A grep. That target
# already owns this verification, and it checks TWO things this Dockerfile must
# not restate:
#
#   1. the build still REACHES the views — it rebuilds into a scratch file and
#      asserts every class in CSS_REQUIRED_CLASSES is present. Each of those
#      classes is written in exactly one place and is reachable through exactly
#      one content entry, so each is a positive control for one glob rather than
#      a sample of the whole. A zero on any of them is the silent case.
#   2. the COMMITTED web/static/app.css is byte-identical to what those views
#      produce (`cmp`, not a parsed diff summary — parsing another tool's output
#      makes its format a dependency). A stale committed stylesheet would embed
#      cleanly and serve last month's classes.
#
# Restating either check here would be a second copy of a predicate, which this
# project's own notes record as wrong at N-1 of N sites. The copy that would rot
# is this one: the Makefile is what contributors run.
#
# ⚠ TAILWIND IS PINNED BY package.json AND INVOKED THROUGH IT. The Makefile's
# TAILWIND defaults to `npx --yes tailwindcss@3`, which would fetch from the
# network mid-build; overriding it to the installed binary keeps this stage
# hermetic and keeps the version the same one `npm install` resolved.
RUN make css-check TAILWIND=./node_modules/.bin/tailwindcss

# --- Stage 2: build the static Go binaries ----------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The verified stylesheet, so //go:embed web/static succeeds against the file
# stage 1 proved rather than the one that happened to be in the context.
COPY --from=css /app/web/static/app.css ./web/static/app.css
ARG VERSION=dev

# The server.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/ZacxDev/muster/internal/api.BuildVersion=${VERSION}" \
    -o /muster-server ./cmd/muster-server

# The agent-side CLI, served to dispatched agents over GET /agent/muster so they
# can work their task with `muster agent task …` instead of hand-rolled curl.
#
# 🔴 BUILT HERE, FROM THE SAME SOURCE TREE — BUT NOT STAMPED WITH ${VERSION},
# AND THE DIFFERENCE IS VISIBLE TO AN OPERATOR. The `ldflags` below carry no
# `-X`, so this binary keeps `main.buildVersion`'s in-source default and
# `muster --version` inside an agent answers `dev` while the server it talks to
# answers ${VERSION} from /health. The two are the same SOURCE by construction;
# they are not the same reported version, and `GET /agent/muster` advertises
# `X-Muster-Version: api.BuildVersion` — the SERVER's value, which describes the
# serving process and not these bytes (see internal/api/agent_cli.go).
#
# The practical consequence: every agent that fetches this CLI gets warnSkew's
# version note on every command, because the client says `dev` and the server
# says a release. Closing that means adding
# `-X main.buildVersion=${VERSION}` here, with a CI step asserting the in-image
# binary reports it; that is deliberately a separate change, because the
# assertion is red until the stamp lands.
#
# CGO_ENABLED=0 is load-bearing: the target is the agent's own image, not this
# Alpine builder, so the binary must carry no dynamic loader dependency.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w" \
    -o /muster ./cmd/muster

# The migrator, so a deployment can run the schema step without a second image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" -o /muster-migrate ./cmd/muster-migrate

# --- Stage 3: minimal runtime ------------------------------------------------
# distroless/static includes CA certificates, which the GitHub calls need.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /muster-server /muster-server
COPY --from=build /muster-migrate /muster-migrate
# 🔴 THE PATH IS api.defaultAgentCLIPath, AND THE CONSTANT'S COMMENT SAYS THE
# LAYOUT IS SET "IN THE SAME REPO AS THIS CONSTANT". This line is what makes
# that sentence true. Read at request time by GET /agent/muster; never executed
# in this image.
COPY --from=build /muster /agent-cli/muster
# MUSTER_PORT's default. See cmd/muster-server/config.go.
EXPOSE 8105
USER nonroot:nonroot
ENTRYPOINT ["/muster-server"]
