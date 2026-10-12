{
  description = "muster — an agent work queue and dispatcher: tasks, provisioning, and a fleet chief";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      # Darwin is listed because everything here is pure Go and has no reason
      # not to build there. The IMAGE is Linux-only, and is exposed only on
      # Linux rather than failing at evaluation with a `dockerTools` error.
      #
      # 🔴 `x86_64-darwin` IS DELIBERATELY ABSENT, AND ITS ABSENCE IS
      # LOAD-BEARING. The nixpkgs this flake locks DROPPED that platform, so
      # listing it makes `nix flake show` and `nix flake check --all-systems`
      # exit 1 for the WHOLE flake, not just for that attribute — i.e. the two
      # canonical "is this flake healthy" commands go red while every CI job,
      # which builds explicit `x86_64-linux` attributes, stays green and cannot
      # see it. Re-add it only against a nixpkgs that still has it.
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      linuxSystems = [ "x86_64-linux" "aarch64-linux" ];

      forSystems = list: f: nixpkgs.lib.genAttrs list (system: f nixpkgs.legacyPackages.${system});
      forAll = forSystems systems;

      # 🔴 THE VERSION IS DERIVED, NEVER A LITERAL. A hand-maintained version
      # string is a claim nobody re-checks: it keeps its old value across the
      # commit that should have moved it, so an artefact ships mislabelled while
      # looking right, and every later reading of "which code is this" is wrong.
      # A git revision cannot disagree with the tree it was built from.
      # `dirtyShortRev` appears when the build came from a modified working
      # tree, which is itself the fact a reader of the tag most wants.
      version = self.shortRev or self.dirtyShortRev or "unknown";

      # 🔴 AN ALLOWLIST, NOT AN EXCLUDE LIST. A working tree of this repo will
      # hold caches, editor droppings and — the day someone runs the suite
      # against a local dump — data. An exclude list ships whatever nobody
      # thought to name. Flake source is already git-filtered, so untracked
      # files are not copied; but that is a property of how the source was
      # FETCHED, not of this derivation, and it does not hold for
      # `nix build path:.`. Stating the filter here makes the property true of
      # the derivation itself.
      #
      # 🔴 `migrations/*.sql` IS IN THIS FILTER AND ITS ABSENCE WOULD BE A BUILD
      # FAILURE, WHICH IS THE GOOD DIRECTION AND STILL WORTH NAMING. The files
      # are `//go:embed`ed by `internal/db`, and `loadMigrations` REFUSES an
      # embed that matched nothing — so a filter carrying only `.go` files fails
      # the build's own check phase with a message about the embed pattern, not
      # about the filter. Same class as `go.sum` below.
      #
      # 🔴 `web/` IS IN THIS FILTER BECAUSE WITHOUT IT NOTHING IN THIS FLAKE
      # BUILT AT ALL, AND THAT WENT UNNOTICED FOR THE WHOLE LIFE OF THE FLAKE.
      # Measured at 1bbef4d, before this line existed:
      #
      #     go: github.com/ZacxDev/muster/internal/api imports
      #             github.com/ZacxDev/muster/web: no required module provides
      #             package github.com/ZacxDev/muster/web
      #
      # — `nix build .#muster-migrate` failed in the *module-fetch* derivation,
      # i.e. the flake's only package, its `default`, its `app` and its only
      # `check` were ALL red on `main`. Nothing caught it because no CI job
      # builds a nix output (see the `nix` job added in ci.yml with this change,
      # which is what makes this filter's correctness observable).
      #
      # The mechanism is the generic hazard of an allowlist: `internal/api`
      # imports the root-level `web` package for its `//go:embed static`, and a
      # filter enumerating `cmd/` and `internal/` cannot see a dependency that
      # lives outside both. `go build ./...` on a real checkout is blind to it
      # because a real checkout has every file.
      #
      # ⚠ `web/static/**` IS CARRIED AS *BYTES*, NOT AS `.go` FILES — the only
      # rows in this filter that are. `//go:embed static` fails the build if the
      # directory is absent or empty, so narrowing this to `.go` would swap one
      # missing-package error for one missing-embed error. `web/css/input.css`
      # is deliberately NOT here: it is Tailwind's *input*, consumed by
      # `make css`, and no Go package reads it.
      onlyGo = pkgs: pkgs.lib.cleanSourceWith {
        src = ./.;
        name = "muster-source";
        filter = path: type:
          let rel = pkgs.lib.removePrefix (toString ./. + "/") (toString path);
          in
          (type == "directory" && (
            rel == "cmd" || rel == "internal" || rel == "web"
            || pkgs.lib.hasPrefix "cmd/" rel || pkgs.lib.hasPrefix "internal/" rel
            || rel == "web/static" || pkgs.lib.hasPrefix "web/static/" rel
          ))
          || (rel == "web/static.go")
          || (pkgs.lib.hasPrefix "web/static/" rel)
          || (rel == "go.mod")
          # 🔴 `go.sum` IS NAMED EXPLICITLY. `buildGoModule` with a real
          # `vendorHash` needs the lock file to resolve the module graph, so a
          # filter carrying `go.mod` alone stops the fetch phase dead. Spelled
          # as its own row rather than a `go.*` glob, for the reason the `.go`
          # row below gives: a glob is an allowlist that says something wider
          # than it means.
          || (rel == "go.sum")
          # 🔴 `.js` IS HERE FOR THE SAME REASON `.sql` IS: IT IS `//go:embed`ED.
          # `internal/ui/components.go` embeds `js/*.js`, and `cmd/muster`'s tests
          # import `internal/api`, which imports `internal/ui` — so without this
          # row the CLI's CHECK phase dies with
          #
          #     internal/ui/components.go:2607:12: pattern js/filter-toggle.js: no matching files found
          #
          # — i.e. a filter row omitted here surfaces as a compile failure naming
          # the embed, never naming the filter. That is the good direction and it
          # is still a trap: the message points at the code.
          #
          # ⚠ `.sh` IS DELIBERATELY NOT CARRIED. `internal/agentspec/autosave.go`
          # embeds `autosave.sh`, and no derivation here builds, vets or tests
          # that package. The day one does, it fails to compile with
          # `pattern autosave.sh: no matching files found` — add the suffix then.
          #
          # ⚠ WHEN A NEW EMBEDDED FILE TYPE APPEARS IN A PACKAGE A DERIVATION
          # BUILDS OR TESTS, IT MUST BE ADDED HERE. There is no glob doing it for
          # you, deliberately — see the `go.sum` note. The symptom is the message
          # above.
          || ((pkgs.lib.hasPrefix "cmd/" rel || pkgs.lib.hasPrefix "internal/" rel)
              && (pkgs.lib.hasSuffix ".go" rel || pkgs.lib.hasSuffix ".sql" rel
                  || pkgs.lib.hasSuffix ".js" rel))
          # 🔴 `testdata/` IS CARRIED WHOLE, AND IT IS THE ONE ROW HERE THAT IS
          # NOT AN EXTENSION. ONE file under it is read by a test a `checkPhase` here
          # RUNS: `cmd/muster/one_service_test.go` opens
          # `../../internal/api/testdata/routes.golden`, so without this row the
          # CLI derivation fails in `TestEveryWiredRouteIsServedByThisProject` and
          # `TestTheSkewProbeTargetIsObservedAndServed`. Every other `testdata/`
          # directory it carries belongs to a package no derivation here tests.
          # The row stays a directory rather than that one path: `testdata` is a
          # name Go's own tooling defines, so it is the stable thing to name.
          #
          # ⚠ THIS IS THE ONE PLACE THE ALLOWLIST IS WIDER THAN A FILE LIST, so
          # it is the one place the header's hazard applies: a local dump dropped
          # under a `testdata/` directory WOULD be copied into the store. It
          # would also have to be `git add`ed to get there via a flake fetch, and
          # a reviewer sees that.
          || (pkgs.lib.hasInfix "/testdata/" rel
              && (pkgs.lib.hasPrefix "cmd/" rel || pkgs.lib.hasPrefix "internal/" rel));
      };

      # 🔴 THE GO TOOLCHAIN IS PINNED, NOT INHERITED. `pkgs.go` follows nixpkgs
      # and will drift ahead of what `go.mod` declares and what CI's
      # `go-version-file` resolves — so the default would build muster under a
      # compiler nothing in this repo had ever run the tests under, and a lock
      # bump would move it again silently. Move this, `go.mod`'s `go` directive
      # and the CI job's toolchain together or not at all.
      #
      # 🔴 IT IS `buildGo125Module`, NOT `buildGoModule` WITH THE COMPILER IN
      # `nativeBuildInputs`. That spelling is a NO-OP for the pin:
      # `buildGoModule` uses the `go` from its OWN scope, so the build would
      # fetch whatever nixpkgs' default is while the derivation advertised 1.25
      # in its inputs — a pin that reads as one and is not.
      #
      # ⚠ `go_1_25` TRACKS A SERIES, NOT A PATCH RELEASE. The property this pin
      # holds is the MINOR version, which is what a language-version skew would
      # break.
      buildGoPinned = pkgs: pkgs.buildGo125Module;

      # 🔴 ONE BINDING, READ BY EVERY GO DERIVATION, BECAUSE THERE IS ONE
      # MODULE. Several literals would be several chances for one to go stale,
      # and a stale vendor hash is a build failure whose message points at the
      # derivation rather than at the `go.mod` change that caused it.
      #
      # To move it: change the dependency, set this to `lib.fakeHash`, run
      # `nix build .#muster-migrate`, and copy the hash nix prints.
      #
      # ⚠ A VENDOR HASH IS NOT A DEPENDENCY GATE AND MUST NOT BE READ AS ONE. It
      # pins the BYTES of whatever the module graph resolves to; it says nothing
      # about which modules are in that graph, and adding one just changes the
      # hash.
      #
      # 🔴 THE PREVIOUS LITERAL HERE WAS NEVER CORRECT AND COULD NOT HAVE BEEN.
      # The module-fetch derivation it is the hash OF failed before it produced
      # anything (`web` was outside `onlyGo` — see the filter above), so nix never
      # reached the comparison and the value was never contradicted. A hash that
      # is never checked is indistinguishable from a right one. It is replaced
      # here with the value nix printed for the first fetch that actually
      # succeeded, and the `nix` CI job added with this change is what will
      # contradict it next time.
      goVendorHash = "sha256-e+cVpoLIOAjIQwlNF+joabU48IBzlcRFDM9cOIKiWrk=";

      # goCheckPhase builds a check phase for one of this flake's Go
      # derivations. `testPaths` is the package pattern `go test` is given.
      #
      # 🔴 IT IS SPELLED OUT RATHER THAN LEFT TO THE DEFAULT, BECAUSE THE DEFAULT
      # CHECK PHASE HONOURS `subPackages` AND THAT MAKES IT VACUOUS. The
      # mechanism, which is the part worth understanding rather than copying:
      # `subPackages` exists to narrow what gets INSTALLED, and
      # `buildGoModule`'s default `checkPhase` reuses it to narrow the TEST WALK
      # too. With `subPackages = [ "cmd/muster-migrate" ]` the walk covers
      # exactly one directory — and `cmd/muster-migrate` contains NO test files
      # at all, so the build log reads
      #
      #     Running phase: checkPhase
      #     ?  .../cmd/muster-migrate  [no test files]
      #     Running phase: installPhase
      #
      # — a GREEN check phase that ran ZERO tests. That is a reassuring zero
      # arriving through a build option whose only documented job is something
      # else. (Measured on THIS module, not inherited from the project the style
      # came from: `go test ./cmd/muster-migrate/...` prints exactly that line.)
      #
      # ⚠ AND THE TELL DOES NOT SURVIVE. The `[no test files]` line is what makes
      # it visible. The day `cmd/muster-migrate` gains one test, the same
      # narrowing would run ONE package and print a plausible-looking PASS, with
      # nothing in the log to say the others never ran. The `ok` COUNT below is
      # the instrument that does not depend on that tell.
      #
      # 🔴 `go test` COVERS A NAMED SET, AND THIS IS A NARROWING THAT HAS TO BE
      # JUSTIFIED RATHER THAN INHERITED.
      # `go test ./...` here is NOT the stronger option — it is a RED one, and
      # that was measured rather than supposed. Four packages fail inside the
      # sandbox for reasons that have nothing to do with the code under test:
      #
      #   internal/agentspec   — its autosave tests shell out to `git`, which the
      #                          sandbox has no binary for
      #   internal/modulegate  — reads Dockerfile, .dockerignore and
      #                          tailwind.config.js, i.e. REPOSITORY files that a
      #                          filtered `src` deliberately does not carry
      #   internal/notes       — reads the root-level testdata/ golden vectors
      #   internal/ui          — reads tailwind.config.js and web/css/input.css
      #
      # Every one of those asserts a property of the REPOSITORY, not of the
      # compiled packages, so a derivation whose source is an allowlisted subset
      # structurally cannot run them. Carrying enough of the tree to satisfy them
      # would mean carrying essentially all of it, which is the filter's whole
      # point undone. So the rule here is: a derivation's check phase tests THE
      # PACKAGES IT INSTALLS.
      #
      # 🔴 AND `go vet` IS GIVEN THE SAME `testPaths`, NOT `./...`. A consumer
      # pinning this flake gets the packages it installs vetted; the
      # whole-module vet is the CI `build` job's. So a vet-only defect in a
      # package outside `testPaths` does NOT fail `nix build`.
      #
      # 🔴 WHAT THIS PHASE STRUCTURALLY CANNOT SEE, so nobody reads a green nix
      # build as a green suite:
      #   * the four packages above — covered ONLY by the CI `test` job, which
      #     runs against a real checkout with `git` on PATH
      #   * every Postgres-backed test. The sandbox has no network and no
      #     database and `MUSTER_TEST_REQUIRE_DB` is unset, so they SKIP. That is
      #     deliberate — a build that needed a database would not be a build —
      #     but it means this phase covers the pure-Go half only.
      #   * races. CI runs `-race`; this does not, to keep a package build cheap.
      goCheckPhase = testPaths: ''
        runHook preCheck

        go vet ${testPaths}

        # 🔴 A COUNT, NOT AN EXIT CODE. `go test` exits 0 when every package it
        # was given has no test files, so the status alone cannot tell "the suite
        # passed" from "there was no suite". This asserts at least one `ok` line,
        # which is the smallest thing that cannot be true of a vacuous run — the
        # same reasoning as the CI job's skip accounting, one level cruder.
        #
        # ⚠ The output is captured and then printed rather than piped, so the
        # build log still shows everything while `go test`'s own exit status
        # stays readable. A `| tee` would hand us tee's status instead.
        if ! go test ${testPaths} > go-test.log 2>&1; then
          cat go-test.log
          echo "checkPhase: go test ${testPaths} FAILED" >&2
          exit 1
        fi
        cat go-test.log
        if ! grep -qE '^ok ' go-test.log; then
          echo "🔴 checkPhase: go test ${testPaths} passed WITHOUT RUNNING A SINGLE" >&2
          echo "   TEST PACKAGE. Every package matched reported [no test files], so this" >&2
          echo "   phase proved nothing. Widen the paths or delete the claim." >&2
          exit 1
        fi
        rm -f go-test.log

        runHook postCheck
      '';

      mkMigrate = pkgs: (buildGoPinned pkgs) {
        pname = "muster-migrate";
        inherit version;
        src = onlyGo pkgs;
        vendorHash = goVendorHash;

        subPackages = [ "cmd/muster-migrate" ];

        # 🔴 THE UNIT TESTS RUN IN THE BUILD, NOT ONLY IN `checks`. A consumer
        # that pins this flake builds the PACKAGE and never runs `nix flake
        # check`, so a broken migration loader would otherwise reach a machine —
        # and `checks` is also where a `--no-link` CI job is easiest to forget.
        doCheck = true;

        # The reasoning behind this narrowing, and everything it cannot see, is
        # at `goCheckPhase` above — stated once, because both derivations here
        # make the same trade and two copies would be two chances to drift.
        #
        # `internal/db` is in scope and `cmd/muster-migrate` alone would not be:
        # the binary is a thin main() over the migration loader, which is where
        # every test that matters to it lives. Without `internal/db` this phase
        # would be the vacuous zero the comment above describes.
        checkPhase = goCheckPhase "./cmd/muster-migrate/... ./internal/db/...";

        meta = with pkgs.lib; {
          description = "Apply muster's schema migrations to a Postgres database";
          homepage = "https://github.com/ZacxDev/muster";
          license = licenses.mit;
          mainProgram = "muster-migrate";
          platforms = platforms.unix;
        };
      };

      # ---------------------------------------------------------------------
      # THE CLI.
      #
      # 🔴 WHY THIS OUTPUT EXISTS, STATED AS THE FAILURE IT CLOSES RATHER THAN AS
      # A FEATURE. `cmd/muster` shipped in-tree with no build output and nothing
      # that installed it, so every consumer reached for the CLI from the project
      # muster was extracted from instead — a DIFFERENT repository, PRIVATE, with
      # its own release cadence. The two then drifted on the provenance headers:
      # the other client sent one spelling, muster's API read the other, and for
      # six days every task comment was attributed to the generic API caller
      # while the task↔session thread recorded NOTHING. The server now accepts
      # both spellings, but that is the symptom's fix. The cause is that the
      # client lived somewhere its server could not gate it.
      #
      # 🔴 AND THE REASON A PACKAGE HELPS IS THAT THIS REPOSITORY IS PUBLIC. The
      # other client can only be packaged from a LOCAL PATH, because fetching a
      # private source would put a credential in the nix store — and a local-path
      # derivation's `vendorHash` is only correct for whichever checkout the host
      # happens to hold, which broke two machines in OPPOSITE directions on one
      # commit. A public flake output is FETCHABLE and pinnable by revision, so a
      # downstream consumer gets the whole class deleted rather than worked
      # around.
      #
      # ⚠ THE BINARY IS `muster` AND THE OUTPUT IS `muster-cli`, DELIBERATELY NOT
      # THE SAME WORD. `cmd/muster` compiles to a binary named `muster` and
      # `mainProgram` must say so or `lib.getExe` points at a path that does not
      # exist. The ATTRIBUTE is a different question: `packages.muster` would be
      # the obvious name for this project's SERVICE (`cmd/muster-server`, which
      # has no output yet and will want one), and reading `packages.muster` as
      # "the client" is a trap worth not setting. `muster-cli` names the thing.
      #
      # ⚠ `cmd/muster/progname.go` MAKES THE BINARY'S OWN NAME NEGOTIABLE, which
      # is what frees this choice. The binary reads `argv[0]` and calls itself
      # whatever it was invoked as — help text, error prefix and version-skew
      # note all follow — precisely so a host can symlink it under a transitional
      # alias and keep command-line-matching hooks armed. So a consumer is not
      # stuck with `muster`: install this and symlink. What that code does NOT do
      # is change behaviour per name — same command tree, same config, same
      # service.
      mkCLI = pkgs: (buildGoPinned pkgs) {
        pname = "muster-cli";
        inherit version;
        src = onlyGo pkgs;
        vendorHash = goVendorHash;

        subPackages = [ "cmd/muster" ];

        # ---------------------------------------------------------------------
        # 🔴 THE LINK-TIME STAMP — THE AUTHORITATIVE "WHY" FOR THE BUILD
        # DECISION LIVES HERE: why there is an `ldflags` at all, why the target
        # is `main.buildRevision`, and why the binding is the same `version`
        # that names the derivation. `tests/cli-version-stamp.sh` is the check
        # and points at this block rather than restating it; previously the same
        # rationale was written out in both files and in the commit that added
        # them, so three copies could drift from one line of `ldflags`. That is
        # now two, and the headline says "the build decision" rather than
        # "nowhere else" because the second copy is deliberate:
        #
        # ⚠ `cmd/muster/client.go`'s `buildRevision` DECLARATION STATES THE SAME
        # PROVENANCE STORY, ON PURPOSE — DO NOT DELETE IT AS A DUPLICATE. It
        # answers a different question at the place a reader asks it: why the
        # VARIABLE exists and why it is separate from `buildVersion`, which is a
        # Go reader's question and not a packaging one. An earlier wording of
        # this headline claimed the rationale lived nowhere else, which was
        # false in exactly the way that invites someone to delete that block and
        # leave a Go declaration whose reason is only in a build file. The
        # shared part is the derivation-name/`readlink -f` anecdote; if the two
        # ever disagree, THIS block is authoritative for the build and
        # client.go's is authoritative for the variable split.
        #
        # WITHOUT THE STAMP THE ARTEFACT COULD NOT NAME ITSELF. `mkCLI` set no
        # `ldflags`, so every nix-built CLI kept the Go default and
        # `muster --version` answered
        #
        #     muster version dev
        #
        # for a binary whose own store path read `muster-cli-4b5d128`. The
        # revision a consumer fetched was recorded in the DERIVATION NAME and
        # nowhere the program could reach it, so "which muster is installed
        # here?" was answerable only by someone who knew to run `readlink -f` on
        # the binary — a provenance question asked during incidents, by people
        # holding the artefact and not the store.
        #
        # 🔴 IT IS THE SAME `version` BINDING THAT NAMES THE DERIVATION, ON
        # PURPOSE AND NOT BY COINCIDENCE. One source means the two can never
        # disagree; a second literal here would be a second chance for one to go
        # stale, and a binary that reports a different revision from the store
        # path it lives in is worse than one that reports nothing, because it
        # answers the provenance question WRONGLY. `installCheckPhase` below
        # asserts the binary's own output against this value.
        #
        # 🔴 THE TARGET IS `main.buildRevision`, NOT `main.buildVersion`, AND
        # THAT DISTINCTION IS THE WHOLE REASON THE SECOND VARIABLE EXISTS. This
        # binding is a GIT REVISION. `buildVersion` is the muster SERVER version
        # the client was built against: it is the value `warnSkew` COMPARES
        # against a live `/health`, and the unlabelled first half of every line
        # `cliVersion()` renders. Stamping a revision there made the skew note
        # and the route-absent 404 render "server 0.2.2 … built against
        # ba6698e" — a comparison that cannot be made, on the one surface where
        # an operator is reasoning about compatibility — and it would make the
        # note fire against every server forever.
        #
        # All three readers — `--version`, the route-absent 404 and the skew
        # note — now render `cliVersion()`, which is the single place the two
        # values meet and LABELS them: `muster version dev (rev ba6698e)`. The
        # revision is on those surfaces on purpose; what the split buys is that
        # it arrives labelled and does not displace the server pin that
        # `cmd/muster/server_pins_test.go` holds to `api.BuildVersion`'s default.
        # `tests/cli-version-stamp.sh` refuses a revision-SHAPED server half.
        #
        # 🔴 THE SYMBOL PATH IS `main`, NOT `github.com/ZacxDev/muster/cmd/muster`.
        # `cmd/muster` IS a main package, so that is where the linker looks for
        # the variable. This matters because `-X` on a path that resolves to
        # nothing is ACCEPTED SILENTLY — no warning, no failure, just an
        # unstamped binary — which is the same observable as having no `ldflags`
        # at all. The `installCheckPhase` assertion is what tells those apart.
        #
        # ⚠ `internal/api.BuildVersion` IS DELIBERATELY NOT STAMPED HERE. The CLI
        # does not link internal/api (see cmd/muster/server_pins_test.go — that
        # import is test-only, and keeping it so is the project's central claim),
        # so a `-X` against it would be exactly the silently-ignored no-op the
        # paragraph above warns about. The server's version travels with the
        # server: `make image` passes VERSION, per the note on that target.
        # ---------------------------------------------------------------------
        ldflags = [ "-X main.buildRevision=${version}" ];

        # Same reasoning as the migrate package: a consumer pinning this flake
        # builds the PACKAGE and never runs `nix flake check`.
        doCheck = true;

        # `internal/taskstatus` is the only non-stdlib package `cmd/muster`
        # imports, so these two directories ARE the CLI. Unlike the migrate
        # binary, `cmd/muster` carries its own suite — including the verb ledger
        # in tree_test.go — so this is a real test walk rather than a formality.
        checkPhase = goCheckPhase "./cmd/muster/... ./internal/taskstatus/...";

        # 🔴 THE VERB LEDGER, RUN AGAINST THE INSTALLED BINARY. This is the ONE
        # check that can see whether this derivation packaged the CLI at all:
        # `subPackages` pointing at the wrong directory, or a wrong
        # `mainProgram`, leaves every Go test green while the artefact a consumer
        # fetches is some other program. tree_test.go walks the cobra tree
        # in-process and is blind to packaging by construction; this walks
        # `$out/bin/muster`.
        #
        # It is in the PACKAGE rather than in a separate `checks` attribute for
        # the same reason `doCheck` is: downstream tooling is being repointed at
        # this binary for the task/agent surface, and the guard has to travel
        # with the thing being consumed.
        #
        # ⚠ IT IS REFERENCED AS A STORE PATH, NOT THROUGH `src`. The script is
        # not in `onlyGo`'s allowlist and does not need to be — `${./…}` copies
        # that one file, which also means editing it rebuilds this derivation.
        # `bash` explicitly, because the file's `/usr/bin/env` shebang does not
        # resolve inside the sandbox.
        #
        # ⚠ `doInstallCheck` DOES NOT RUN WHEN CROSS-COMPILING, so a cross build
        # of this package is NOT covered by it. Native builds are.
        # ⚠ THE TWO SCRIPTS STAY SEPARATE AND MERGING THEM WOULD BREAK CI —
        # stated once, in `tests/cli-version-stamp.sh`'s header, because the
        # person who would merge them is editing that file.
        #
        # 🔴 IT IS ASSERTED ON THE BINARY'S OWN OUTPUT, NOT ON THE DERIVATION
        # NAME. The name is what was already right and already load-bearing (it
        # is how the installed revision was identified before this change, and it
        # still works); reading it here would re-assert nix's own naming and
        # would stay green for a binary with no `ldflags` at all — which is the
        # exact defect. `$out/bin/muster --version` is the only witness to
        # whether the override reached the artefact a consumer runs.
        doInstallCheck = true;
        installCheckPhase = ''
          runHook preInstallCheck
          bash ${./tests/verb-ledger.sh} "$out/bin/muster"
          bash ${./tests/cli-version-stamp.sh} "$out/bin/muster" ${pkgs.lib.escapeShellArg version}
          runHook postInstallCheck
        '';

        meta = with pkgs.lib; {
          description = "muster's own machine client for its JSON API (tasks, agents, chief)";
          homepage = "https://github.com/ZacxDev/muster";
          license = licenses.mit;
          mainProgram = "muster";
          platforms = platforms.unix;
        };
      };

      # muster-mirror: the OPTIONAL host half of the mirrored-sessions plugin. It
      # runs on an operator's own machine (never in the cluster), lists that
      # machine's tmux sessions and dials OUT to muster. Install it only on the
      # hosts whose sessions muster should show:
      #
      #     nix profile install github:ZacxDev/muster#muster-mirror
      #
      # (or `go install github.com/ZacxDev/muster/cmd/muster-mirror@<rev>`).
      # tmux itself is the host's own; it is deliberately not a runtime input,
      # because the sessions to mirror live on the host's tmux server.
      mkMirror = pkgs: (buildGoPinned pkgs) {
        pname = "muster-mirror";
        inherit version;
        src = onlyGo pkgs;
        vendorHash = goVendorHash;

        subPackages = [ "cmd/muster-mirror" ];

        ldflags = [ "-X main.buildRevision=${version}" ];

        doCheck = true;

        # The untagged tests only: the real-tmux suite (`-tags tmuxit`) runs in
        # CI's ccd-tmux job, and the nix sandbox has no tmux.
        checkPhase = goCheckPhase "./cmd/muster-mirror/... ./internal/mirrorhost/...";

        meta = with pkgs.lib; {
          description = "Show this machine's tmux sessions in muster as mirrored sessions (dials out; no listener; read-only unless -interactive)";
          homepage = "https://github.com/ZacxDev/muster";
          license = licenses.mit;
          mainProgram = "muster-mirror";
          platforms = platforms.unix;
        };
      };

      # 🔴 THE IMAGE CARRIES BUSYBOX AND DECLARES `PATH`, AND NEITHER IS
      # DECORATION. `buildLayeredImage` with `contents` set to the binary alone
      # produces an image with NO `PATH` and no `sh` — so `kubectl exec … -- sh`
      # and every `docker run --entrypoint sh` lands on "executable file not
      # found in $PATH" against an image that otherwise starts and works. For a
      # one-shot migration tool that is exactly when you need a shell: the run
      # failed and you want to look at the database.
      #
      # 🔴 CA ROOTS TOO, AND THE FAILURE WITHOUT THEM IS A CONFUSING ONE. A
      # managed Postgres reached over TLS (`sslmode=require`) needs a trust
      # store, and this image ships no `/etc` of its own — so the failure is a
      # certificate-verification error on a DSN that is entirely correct.
      #
      # ⚠ `Cmd` NAMES THE BINARY BY ABSOLUTE STORE PATH. `/bin/muster-migrate`
      # would also resolve, and that is the reason not to use it: the entrypoint
      # would then depend on `contents` placing the package's `bin/` at the image
      # root AND on `PATH`, so a change to either turns a wiring mistake into a
      # container that does not start.
      mkMigrateImage = pkgs:
        let migrate = mkMigrate pkgs;
        in
        pkgs.dockerTools.buildLayeredImage {
          name = "muster-migrate";
          tag = version;
          contents = [ migrate pkgs.busybox pkgs.cacert ];

          config = {
            Cmd = [ "${pkgs.lib.getExe migrate}" ];
            Env = [
              "PATH=/bin"
              "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
            ];
            User = "65532:65532";
            WorkingDir = "/";
          };
        };
    in
    {
      packages = forAll (pkgs:
        {
          muster-migrate = mkMigrate pkgs;
          # 🔴 `muster-cli` IS AN ADDITION AND `default` IS UNTOUCHED, WHICH IS A
          # DECISION AND NOT AN OVERSIGHT. See the note on `apps` below: the two
          # `default`s resolve a bare `nix run`/`nix build` of this flake, moving
          # one alone gives one name two binaries, and nothing about shipping the
          # client asks for the flake's unnamed entry point to change meaning.
          # Consumers address it by name.
          muster-cli = mkCLI pkgs;
          muster-mirror = mkMirror pkgs;
          default = mkMigrate pkgs;
        }
        // nixpkgs.lib.optionalAttrs
          (builtins.elem pkgs.stdenv.hostPlatform.system linuxSystems)
          {
            migrate-image = mkMigrateImage pkgs;
          });

      # 🔴 `apps.default` AND `packages.default` MOVE TOGETHER OR NOT AT ALL.
      # `nix run github:…/muster` resolves `apps.default` FIRST and only falls
      # back to `packages.default`'s `mainProgram`, so flipping one alone gives
      # ONE name two binaries — which one you get depends on the command you ran.
      apps = forAll (pkgs: {
        default = {
          type = "app";
          program = "${nixpkgs.lib.getExe (mkMigrate pkgs)}";
        };
        muster-migrate = {
          type = "app";
          program = "${nixpkgs.lib.getExe (mkMigrate pkgs)}";
        };
        # `nix run github:ZacxDev/muster#muster-cli -- task ls`.
        #
        # ⚠ `getExe` RESOLVES THROUGH `meta.mainProgram`, which for this package
        # is `muster` and not the attribute name. That is why `mainProgram` is
        # stated explicitly in `mkCLI` rather than left to default from `pname`:
        # `pname` is `muster-cli`, `$out/bin/muster-cli` does not exist, and
        # `getExe` would point this app at a missing path.
        muster-cli = {
          type = "app";
          program = "${nixpkgs.lib.getExe (mkCLI pkgs)}";
        };
        muster-mirror = {
          type = "app";
          program = "${nixpkgs.lib.getExe (mkMirror pkgs)}";
        };
      });

      checks = forAll (pkgs: {
        muster-migrate = mkMigrate pkgs;
        # The same derivation as `packages.muster-cli`, so this costs nothing
        # beyond making it part of `nix flake check` — which is what the `nix` CI
        # job runs. Its `checkPhase` and its verb-ledger `installCheckPhase` are
        # the substance; this attribute is only how they get collected.
        muster-cli = mkCLI pkgs;
        muster-mirror = mkMirror pkgs;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            # The same toolchain the package and the CI job use — see
            # `buildGoPinned` above for why it is pinned rather than inherited.
            pkgs.go_1_25
            pkgs.git
            pkgs.python3
            # For `make test-db` and the compose file.
            pkgs.docker-client
            # `psql` / `pg_dump`: the schema comparison in CONTRIBUTING.md needs
            # both, and a shell that tells you to run them should carry them.
            pkgs.postgresql_16
          ];
          # 🔴 EVERY GATE, SPELLED OUT VERBATIM, so there is no gap between what
          # the shell says and what CI runs.
          shellHook = ''
            echo "muster dev shell."
            echo
            echo "start the test database (prints the two exports you need):"
            echo "    make test-db"
            echo "the Go suite, with the database REQUIRED rather than skipped:"
            echo "    MUSTER_TEST_REQUIRE_DB=1 go test -race -cover ./..."
            echo "build and vet:"
            echo "    go build ./... && go vet ./..."
            echo "and the leak gate, which must pass before any push —"
            echo "the self-test runs FIRST, and a scanner that cannot go red is testing nothing:"
            echo "    python3 tests/leakscan.py --self-test && python3 tests/leakscan.py"
          '';
        };
      });
    };
}
