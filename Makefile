# muster developer tasks.
#
# Every target here is something CI also runs, spelled the same way. A gate that
# exists only in CI is a gate contributors discover by having it go red.
#
# ⚠ THAT SENTENCE WAS FALSE FOR `css-check` AND IS TRUE AGAIN. Nothing in CI ran
# it, so a stale or thin web/static/app.css could merge with a full green tick —
# and `go:embed` makes that file a BUILD INPUT, so what merges is what ships. The
# `css` job in .github/workflows/ci.yml runs it now, and so does the Dockerfile's
# first stage. The exception that remains, and is deliberate: `test-liveenv`,
# which needs a real coding-agent session in the process environment and cannot
# run on a runner at all.

SHELL := /usr/bin/env bash

COMPOSE ?= docker compose
COMPOSE_FILE := docker-compose.test.yml

# The published port of the throwaway Postgres. 55432 by default, which is what
# README.md, CONTRIBUTING.md and internal/dbtest's doc all spell literally.
#
# 🔴 IT IS A VARIABLE SO TWO CHECKOUTS CAN EACH HAVE THEIR OWN DATABASE, AND IT IS
# `export`ED SO `docker compose` INTERPOLATES THE SAME VALUE THIS FILE USES. Make
# does not pass an unexported variable to a recipe's environment, and compose
# would then silently fall back to its own `:-55432` default — publishing 55432
# while TEST_DSN named the port you asked for. The two must come from one
# variable or they drift, which is the identical failure the compose file's header
# records for POSTGRES_DB.
MUSTER_TEST_PG_PORT ?= 55432
export MUSTER_TEST_PG_PORT

# The DSN docker-compose.test.yml serves. It is stated ONCE, here, and
# `test-db` prints it — two copies is how a recipe drifts into naming the wrong
# database, which is the specific failure that file's header is about.
TEST_DSN := postgres://muster:muster@127.0.0.1:$(MUSTER_TEST_PG_PORT)/muster_test?sslmode=disable

.PHONY: build vet run image hermes-image hermes-smoke test test-db test-db-down test-liveclaude test-livehermes leakscan css css-check check help verb-ledger icons e2e screenshots

# The stylesheet the UI serves, and the classes that prove each Tailwind content
# entry is still matching something. See css-check.
CSS_OUT := web/static/app.css
CSS_IN := web/css/input.css
TAILWIND ?= npx --yes tailwindcss@3

# 🔴 EACH ENTRY IS A POSITIVE CONTROL FOR ONE CONTENT GLOB, NOT DECORATION.
# Every class below is written in exactly one place in the tree and is reachable
# ONLY through `./internal/ui/**/*.go`. Measured with that glob deliberately
# pointed at a non-existent directory: Tailwind exits 0, emits a VALID 7,139-byte
# stylesheet instead of the real one, and every class here drops to zero
# occurrences. That is the whole hazard — there is no error, no warning in the
# exit code, and nothing on a developer's machine looks wrong.
#
# ⚠ THE FULL SIZE IS DELIBERATELY NOT RESTATED HERE ANY MORE. This comment said
# "38,274-byte" and the real file was 38,657 by the time anyone measured — a
# hardcoded figure with nothing deriving it does not stay true, it stays
# written. css-check PRINTS the byte count on every run, which is a producer;
# the 7,139 stays because it is a property of the FAILURE mode (an all-globs-miss
# build), not of this tree, and it is the number that makes the point. (It was
# 5,594 until the palette became custom properties: the token block in
# input.css is base CSS that is emitted whatever the globs match.)
#
# ⚠ PLAIN ALPHANUMERIC CLASSES ONLY, AND THAT IS A HARNESS FIX RATHER THAN A
# TASTE. A responsive or opacity-modified class (`lg:pl-72`, `bg-rose-500/95`)
# is escaped in the CSS as `.lg\:pl-72`, and carrying that backslash through
# Make into a shell into grep's pattern language made grep warn about a "stray \
# before :" and match nothing — a MISSING verdict for a class that was present.
# That is the instrument failing, reported as a finding about the code.
# Each of these is written in a different moved view, so the set spans the glob
# rather than sampling one file.
#
# 🔴 THE PALETTE IS ROLE TOKENS NOW, AND THE FIRST FOUR ARE TOKEN UTILITIES ON
# PURPOSE. A broken `theme.extend.colors` in tailwind.config.js does not error
# either: Tailwind just stops recognising `bg-s1`-style classes and emits a
# stylesheet with no colours at all. `text-accent`, `bg-danger`,
# `bg-st-review-bg` and `text-st-warning-fg` are each written only under
# internal/ui and each needs a different branch of that colour map (a flat role,
# a second flat role, and two different nested status pairs), so a missing role
# or a mangled `st` block fails here instead of shipping.
#
# ⚠ `bg-s1` IS DELIBERATELY NOT A CONTROL even though it is the most common
# surface: login.go writes `bg-s1/70`, and `grep -F bg-s1` matches the escaped
# `.bg-s1\/70` too, so it would stay present with the internal/ui glob broken.
#
# 🔴 THE LAST CLASS COVERS A DIFFERENT CONTENT ENTRY, NOT ANOTHER VIEW.
# `text-balance` is written ONLY in internal/api/login.go — the sign-in page,
# the one document this app emits from outside internal/ui — so it is the
# positive control for that entry. Without it, deleting `./internal/api/login.go`
# from tailwind.config.js would ship a login page with an unwrapped heading
# and every check here would stay green.
CSS_REQUIRED_CLASSES := text-accent bg-danger bg-st-review-bg text-st-warning-fg text-balance

help:
	@echo "muster:"
	@echo "  make build        go build ./... (muster-server, muster, muster-migrate)"
	@echo "  make run          the server, against \$$DATABASE_URL (see README)"
	@echo "  make image        the container image, css-check included"
	@echo "  make hermes-image the hermes agent image (Nix; loads it into Docker)"
	@echo "  make hermes-smoke the hermes image smoke test: a stub model, a full turn, approvals, crash and restart"
	@echo "  make vet          go vet ./..."
	@echo "  make test-db      start the throwaway Postgres, migrate it, print the exports"
	@echo "                    (MUSTER_TEST_PG_PORT=55433 make test-db for a second checkout)"
	@echo "  make test         the Go suite, with the database REQUIRED (not skipped)"
	@echo "  make leakscan     the leak gate, self-test first"
	@echo "  make verb-ledger  prove the BUILT muster CLI exposes exactly its specified verbs"
	@echo "  make css          rebuild $(CSS_OUT) from the Go views"
	@echo "  make css-check    prove the committed stylesheet matches the views"
	@echo "  make check        vet + test + leakscan + css-check"
	@echo "  make e2e          the Playwright suite (needs MUSTER_E2E_DATABASE_URL=…/<name>_e2e)"
	@echo "  make screenshots  regenerate docs/img/*.png from fixture data (same database, plus Docker)"
	@echo "  make test-db-down stop and remove the throwaway Postgres"
	@echo "  make test-liveclaude  answer a REAL Claude Code's AskUserQuestion pickers (run on every Claude Code upgrade)"
	@echo "  make test-livehermes  drive the PINNED Hermes CLI: screen check, Escape, timings, gated send (every pin bump; before -hermes-send)"

build:
	go build ./...

vet:
	go vet ./...

# Run the server. Every MUSTER_* name is spelled once, in
# cmd/muster-server/config.go; DATABASE_URL is required and the binary says so.
run:
	go run ./cmd/muster-server

# 🔴 THE IMAGE BUILD RUNS css-check IN ITS FIRST STAGE, so this target is also
# the cheapest end-to-end proof that the stylesheet the container would serve is
# the one the views produce. VERSION reaches api.BuildVersion, which /health and
# /readyz report.
#
# ⚠ VERSION REACHES THE SERVER ONLY. The agent-side CLI in the same image is
# built from the same tree with no `-X` at all, so it reports `main.buildVersion`'s
# default (`dev`), not this value. Same source, different reported version — see
# the comment on that build step in Dockerfile.
#
# DOCKER_BUILD_FLAGS is passed through verbatim (CI's GHCR publish adds its OCI
# source/revision labels with it); empty by default, so this target is unchanged
# for every other caller.
IMAGE ?= muster-server
VERSION ?= dev
DOCKER_BUILD_FLAGS ?=
image:
	docker build $(DOCKER_BUILD_FLAGS) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# 🔴 `MUSTER_TEST_REQUIRE_DB=1` IS THE POINT OF THIS TARGET, NOT DECORATION.
# Bare `go test ./...` with no database SKIPS every Postgres-backed test and
# reports GREEN — a verdict about a suite that executed none of the store logic,
# indistinguishable in the output from one that executed all of it. This target
# refuses to produce that verdict: without a database it FAILS, naming the
# variable and the way to start one.
#
# ⚠ IT DOES NOT START THE DATABASE FOR YOU, and that is deliberate. A test
# target that silently provisions infrastructure hides how long it takes and
# what it costs, and it makes `make test` unusable against a Postgres you
# already have. Run `make test-db` once; it prints what to export.
test:
	MUSTER_TEST_DATABASE_URL="$${MUSTER_TEST_DATABASE_URL:-$(TEST_DSN)}" \
	MUSTER_TEST_REQUIRE_DB=1 \
	go test -race -cover ./...

# Drive a REAL Claude Code's AskUserQuestion picker (internal/askdrive) in a
# scratch tmux server: every shape of question, the CLI's record checked, the
# redraw lag re-measured. 🔴 RUN IT ON EVERY CLAUDE CODE UPGRADE — the driver
# reads the picker's screen, and a changed layout shows up here first (and in
# muster as an "unrecognised" answer, muster_mirror_ask_typing_total). Needs a
# logged-in `claude` on PATH; spends a few model turns. Never in CI.
test-liveclaude:
	tests/liveclaude.sh

# Drive the PINNED Hermes CLI for real (docs/design/hermes.md, Phase 3): every
# screen state the send's screen check must refuse, what Escape does in each,
# the gated send end to end, (opt-in) the hook/Enter/queue timings, and the
# overlay and vi-label ledgers against the pinned source. 🔴 RUN IT ON EVERY
# HERMES PIN BUMP AND BEFORE ENABLING -hermes-send ON A HOST: it ends with
# "HERMES HARNESS VERDICT: GREEN" or "RED". Isolated: a scratch HERMES_HOME, a
# private tmux server, a local stub model, no credential. Needs `hermes` on
# PATH at the pin and tmux >= 3.7. Never in CI. MUSTER_LIVEHERMES_RECORD=1
# re-records the fixtures (internal/hermeshost/testdata/screens/);
# MUSTER_LIVEHERMES_TIMINGS=1 adds the load timings (hook slack, Enter margin,
# queue gap) — run that on every pin bump.
test-livehermes:
	tests/livehermes.sh

# Start the throwaway Postgres, wait for it, and migrate the TEMPLATE once.
#
# 🔴 THE MIGRATE STEP IS NOT OPTIONAL BOOKKEEPING. internal/dbtest gives each
# test package its own database by `CREATE DATABASE … TEMPLATE`, copying an
# already-migrated schema. Skip this and nothing breaks — but every package
# bootstraps the schema itself, which is the slow path this design exists to
# avoid.
test-db:
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --wait
	DATABASE_URL="$(TEST_DSN)" go run ./cmd/muster-migrate
	@echo
	@echo "Postgres is up and the template is migrated. Export these:"
	@echo
	@echo "    export MUSTER_TEST_DATABASE_URL='$(TEST_DSN)'"
	@echo "    export MUSTER_TEST_REQUIRE_DB=1"
	@echo
	@echo "The second one turns a missing database from a SKIP into a FAILURE."
	@echo "Without it a suite that ran nothing still reports green."

test-liveenv: ## run the live-environment controls (needs a real coding-agent session)
	@# These are behind `//go:build liveenv` because they read their inputs from the
	@# REAL process environment. They are NOT in `make test` on purpose: as t.Skip
	@# they silently no-op'd on CI and the run still looked green.
	go test -tags liveenv -run 'REALProcessEnvironment|SessionIDIsReadableFromThisProcess' -v ./cmd/muster/
	@# The agent-chat controls need a reachable agent runtime, not a coding-agent
	@# session — see internal/agentgateway/liveruntime_test.go for the variables
	@# and the port-forward recipe. They are the closing condition the OWED record in
	@# internal/agents/responses.go wrote for itself: the httptest suite proves what
	@# goes on the wire, and only these prove a runtime accepts it.
	@#
	@# 🔴 MUSTER_LIVE_AGENT_EXPECT_TOOLS DECIDES WHETHER A TOOL-CELL FAILURE IS FATAL —
	@# NOT WHETHER THE CELL RUNS. An earlier heading called it "OPT-IN", which described
	@# a first draft that skipped before doing any work: with a runtime declared that
	@# measured nothing while calling itself a measurement. The cell now RUNS whenever a
	@# runtime is declared and reports what it found; the variable only escalates a
	@# failure to the target. It shipped unconditionally red — the tool
	@# cell cannot pass without a runtime that BOTH speaks the shape agents.ToolDef
	@# sends AND has a working model credential, and no single pod had both — with a
	@# comment here pre-excusing the red. A comment excusing a red gate IS the
	@# click-through training claude/RULES.md names, not a defence against it; and it
	@# also made the TARGET'S EXIT CODE unreadable — the cmd/muster cell runs first and
	@# printed its own verdict fine, so what an always-red cell buried was whether the
	@# target as a whole passed. An earlier wording said "the cell's verdict", which is
	@# measurably not what happened.
	@# So: no runtime declared (no ADDR/TOKEN) => skip, nothing to talk to. Runtime
	@# declared => the cell RUNS and reports. Plus EXPECT_TOOLS => a failure is the
	@# target's failure too.
	go test -tags liveenv -count=1 -run 'RealTurnAgainstALiveRuntime|RealTOOLTurnAgainstALiveRuntime' -v ./internal/agentgateway/

test-db-down:
	$(COMPOSE) -f $(COMPOSE_FILE) down -v

# 🔴 THE SELF-TEST RUNS FIRST, AND THE `&&` IS LOAD-BEARING. A scanner that
# cannot go red reports zero findings exactly the way a clean tree does. The
# self-test proves it can refuse a realistic sensitive string, produce a
# non-zero count, and NOT refuse legitimate content — and only then is the scan
# below a measurement rather than a claim.
leakscan:
	python3 tests/leakscan.py --self-test && python3 tests/leakscan.py

# Render the PWA icons from their SVG sources in web/icon-src/ (the Brass Roll
# concept). The PNGs are committed — they are go:embed inputs — so this runs
# only when a source changes. Needs rsvg-convert (librsvg); on NixOS:
#   nix-shell -p librsvg --run 'make icons'
# The sizes are asserted against the manifest by TestManifestImagesShipAtTheirDeclaredSize.
# ⚠ A GENERATOR, NOT A GATE, so CI does not run it (the header's "every target
# runs in CI" is about checks). The gate is that test, which reads the PNGs.
# The manifest screenshots are generated the same way by `npm run capture` in
# e2e/ (seeded fixture data only).
ICON_SRC := web/icon-src
ICON_OUT := web/static/icons
RSVG ?= rsvg-convert
icons:
	$(RSVG) -w 192 -h 192 $(ICON_SRC)/icon.svg -o $(ICON_OUT)/icon-192.png
	$(RSVG) -w 512 -h 512 $(ICON_SRC)/icon.svg -o $(ICON_OUT)/icon-512.png
	$(RSVG) -w 192 -h 192 $(ICON_SRC)/maskable.svg -o $(ICON_OUT)/maskable-192.png
	$(RSVG) -w 512 -h 512 $(ICON_SRC)/maskable.svg -o $(ICON_OUT)/maskable-512.png
	$(RSVG) -w 72 -h 72 $(ICON_SRC)/badge.svg -o $(ICON_OUT)/badge-72.png
	$(RSVG) -w 96 -h 96 $(ICON_SRC)/shortcut-new.svg -o $(ICON_OUT)/shortcut-new-96.png
	$(RSVG) -w 96 -h 96 $(ICON_SRC)/shortcut-tasks.svg -o $(ICON_OUT)/shortcut-tasks-96.png
	$(RSVG) -w 96 -h 96 $(ICON_SRC)/shortcut-agents.svg -o $(ICON_OUT)/shortcut-agents-96.png

# Rebuild the stylesheet from the Go views.
css:
	mkdir -p $(dir $(CSS_OUT))
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --minify

# 🔴 TAILWIND CANNOT FAIL THE WAY YOU NEED IT TO, SO THIS TARGET FAILS FOR IT.
#
# Tailwind emits rules only for classes it FINDS. A content entry that matches
# nothing is not an error and not a non-zero exit — it silently produces a
# smaller, perfectly valid stylesheet. The page then renders unstyled wherever
# that stylesheet is actually served, which is typically a container and not the
# machine the build ran on, so the first report is "it looks broken in prod".
#
# Two separate claims are checked here, and they fail for different reasons:
#
#   1. THE BUILD STILL REACHES THE VIEWS. Rebuild into a scratch file and assert
#      each class in CSS_REQUIRED_CLASSES is present. A zero here means a content
#      glob stopped matching — the silent case above.
#   2. THE COMMITTED STYLESHEET IS THE ONE THOSE VIEWS PRODUCE. Compare the
#      scratch build against the committed file byte for byte. $(CSS_OUT) is
#      COMMITTED, not gitignored, precisely because a later `go:embed` will make
#      it a build input — and this repository's .gitignore says in its first line
#      that nothing `go build` needs may be ignored, because the project muster
#      came from ignored exactly this file and broke `go build` on a fresh clone.
#      Committing it without this comparison would just move the failure from
#      "missing" to "stale", so both halves ship together.
#
# ⚠ `cmp`, NOT a parsed diff summary. Parsing another tool's output makes its
# FORMAT a dependency; cmp answers the only question here with an exit code.
css-check:
	@set -e; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(TAILWIND) -i $(CSS_IN) -o "$$tmp/app.css" --minify >/dev/null 2>&1; \
	test -s "$$tmp/app.css" || { echo "css-check: the build produced an EMPTY stylesheet"; exit 1; }; \
	for c in $(CSS_REQUIRED_CLASSES); do \
	  grep -qF -- "$$c" "$$tmp/app.css" || { \
	    echo "css-check: '$$c' is MISSING from the built stylesheet."; \
	    echo "  A Tailwind content entry has stopped matching. Tailwind does not"; \
	    echo "  error on this — it emits a smaller valid file and exits 0."; \
	    echo "  Check the content globs in tailwind.config.js."; \
	    exit 1; }; \
	done; \
	test -f $(CSS_OUT) || { echo "css-check: $(CSS_OUT) is not committed — run 'make css'"; exit 1; }; \
	cmp -s "$$tmp/app.css" $(CSS_OUT) || { \
	  echo "css-check: $(CSS_OUT) is STALE — the views have changed since it was built."; \
	  echo "  Run 'make css' and commit the result."; \
	  exit 1; }; \
	echo "css-check: $$(wc -c < $(CSS_OUT)) bytes, all $(words $(CSS_REQUIRED_CLASSES)) control classes present, committed copy matches"

# 🔴 THE VERB SET, ASSERTED AGAINST A BINARY RATHER THAN AGAINST THE SOURCE.
# cmd/muster/tree_test.go pins the same twelve verbs by walking the cobra tree in
# process; it cannot see which binary a BUILD produced. This target compiles
# cmd/muster and interrogates the result, which is the only view that catches a
# build wired to the wrong package. `nix build .#muster-cli` runs the identical
# script as its installCheckPhase — this is the spelling for a checkout with no
# nix.
#
# ⚠ IT BUILDS INTO A TEMPORARY DIRECTORY, NOT THE TREE. A stray `muster` binary
# beside the source is the kind of artefact that gets committed once.
verb-ledger:
	@set -e; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	go build -o "$$tmp/muster" ./cmd/muster; \
	tests/verb-ledger.sh "$$tmp/muster"

# The browser suite, as CI's `e2e` job runs it: playwright, then the verdict
# (pass floor + skip cap) whatever playwright's exit code was. Needs a Postgres
# database whose name ends in _e2e (the seed truncates it) and `npm ci` +
# `npx playwright install chromium` in e2e/ once. CI runs it in legs
# (e2e/legs.json); `MUSTER_E2E_LEG=N make e2e` runs leg N and its leg verdict.
e2e:
	@cd e2e && MUSTER_E2E_REQUIRE_FULL=1 npx playwright test; rc=$$?; \
	node verdict.mjs $${MUSTER_E2E_LEG:+--leg $$MUSTER_E2E_LEG} test-results/results.json && exit $$rc

# Regenerate the documentation's screenshots (docs/img/) from the e2e harness:
# the seeded fixture board and a fake mirrored host, at a phone's size. A
# GENERATOR, NOT A GATE — like `icons`, CI does not run it. Needs what `make e2e`
# needs, plus Docker for the GoTrue container the passkey shot signs in against.
# pngquant (nix-shell -p pngquant) is used when present; every image must end up
# at or under 200 KB either way.
screenshots:
	@cd e2e && MUSTER_E2E_DOCSHOTS=1 npx playwright test
	@if command -v pngquant >/dev/null 2>&1; then pngquant --force --ext .png --quality 65-90 docs/img/*.png; \
	  else echo "screenshots: pngquant not found; images left as captured"; fi
	@for f in docs/img/*.png; do s=$$(wc -c < "$$f"); echo "$$f: $$s bytes"; \
	  test "$$s" -le 204800 || { echo "screenshots: $$f is over 200 KB"; exit 1; }; done

check: vet test leakscan css-check verb-ledger

# The `hermes` agent kind's image (images/hermes-agent/flake.nix): upstream
# Hermes at its pinned sha, ffmpeg overridden out, behind hermesd. The flake's
# output is a stream script, so the image is assembled as Docker loads it.
#
# 🔴 THE hermesd IN THE IMAGE MUST BE THE ONE muster's OWN FLAKE BUILDS. The
# image flake takes muster through a relative `path:` input; the target refuses
# to build when the two derivations differ (a stale or diverged input), rather
# than ship an image whose hermesd is not this tree's.
#
# The tag names Hermes's pin AND hermesd's revision (the flake's `imageTag`).
HERMES_IMAGE_TAG = $(shell nix eval --raw ./images/hermes-agent\#imageTag 2>/dev/null)
hermes-image:
	@root=$$(nix eval --raw .#packages.x86_64-linux.hermesd.drvPath) && \
	img=$$(nix eval --raw ./images/hermes-agent#packages.x86_64-linux.hermesd.drvPath) && \
	if [ "$$root" != "$$img" ]; then \
		echo "hermes-image: the image's hermesd ($$img) is not muster's ($$root): refresh the image flake's muster input (nix flake update --flake ./images/hermes-agent muster)" >&2; exit 1; \
	else echo "hermes-image: hermesd $$root"; fi
	nix build -o .hermes-image ./images/hermes-agent#image
	./.hermes-image | docker load
	rm -f .hermes-image

# The image smoke test: the real image under Docker with pod-like restrictions,
# a stub OpenAI-compatible model and a fake muster relay, driven by muster's own
# client (images/hermes-agent/smoke). It builds and loads the image first, so it
# never tests a stale one. No real model, no Kubernetes.
hermes-smoke: hermes-image
	go run ./images/hermes-agent/smoke -image hermes-agent:$(HERMES_IMAGE_TAG)
