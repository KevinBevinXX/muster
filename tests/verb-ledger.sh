#!/usr/bin/env bash
# The verb ledger, asserted against a BUILT BINARY.
#
# Usage: tests/verb-ledger.sh <path-to-muster-binary>
#
# ---------------------------------------------------------------------------
# 🔴 WHY THIS EXISTS WHEN cmd/muster/tree_test.go ALREADY PINS THE SAME SET.
#
# tree_test.go walks the cobra tree IN PROCESS. It is the stronger check of the
# two — it proves each verb reaches a route — and it is blind to exactly one
# thing: WHICH BINARY A BUILD PRODUCES. Point `subPackages` at the wrong
# directory, get `mainProgram` wrong, or install the server instead of the
# client, and tree_test.go stays green while the artefact a consumer fetches is
# not this CLI at all. That is the seam this file owns: the packaging ↔ verb-set
# relationship, which neither side's own tests can see.
#
# It is also what makes the claim checkable for a consumer who has only the
# artefact: run it against `$(nix build .#muster-cli)/bin/muster`.
#
# 🔴 EXACT EQUALITY, AND IT FAILS WHEN THE SET GROWS *OR* SHRINKS. A `>=` check
# would accept a binary that had gained the whole of the upstream CLI's other
# half (see FORBIDDEN below), and a `<=` check would accept one that had
# silently lost `task comment`. Downstream tooling is being repointed at this
# binary for the task/agent surface, so a dropped verb is a broken session and
# an added one is a split that stopped being a split.
#
# 🔴 WHAT IT STRUCTURALLY CANNOT SEE, so nobody reads a green run as more than
# it is:
#   * It enumerates LEAVES — commands that have no subcommands of their own. A
#     command that is BOTH a group and runnable would not appear. No command in
#     this tree is; tree_test.go's `RunE != nil` walk is the view that would
#     notice if one became so.
#   * It reads `--help` output, so cobra's help FORMAT is a dependency of this
#     script. A format change collapses the parse to an empty set, which fails
#     in the SHRINK direction with a message about the marker — loud, not quiet.
#     The MARKER control below is what makes that distinguishable from a binary
#     that genuinely has no subcommands.
#   * It says nothing about what a verb DOES. tree_test.go asserts the route.
# ---------------------------------------------------------------------------

set -euo pipefail

BIN="${1:-}"
if [ -z "$BIN" ]; then
  echo "usage: $0 <path-to-muster-binary>" >&2
  exit 2
fi
if [ ! -x "$BIN" ]; then
  echo "verb-ledger: $BIN is not an executable file" >&2
  exit 2
fi

# LEDGER is the verb set this binary is specified to have, sorted.
#
# 🔴 CHANGING IT IS A DESIGN DECISION, NOT A TEST FIX — the same sentence
# cmd/muster/tree_test.go opens with, and the two must agree. The extraction
# split the upstream CLI's verbs between two binaries; this half is the
# task/agent surface below. If a thirteenth is genuinely wanted, add it here,
# add it to tree_test.go's `wiring` with the route it reaches, move
# `wantVerbCount`, and say so in the commit.
LEDGER="agent ls
agent messages
agent resolve
agent task comment
agent task get
agent task status
chief ask
task comment
task create
task get
task ls
task status
token create
token ls
token revoke
whoami"

# WANT_COUNT is pinned to the number the split SPECIFIES, not to `wc -l` of
# LEDGER. Both are asserted, so adding a verb AND its ledger row in one commit
# still fails until someone moves this literal deliberately. (tree_test.go pins
# the identical number for the in-process tree; they are two reads of one
# decision.)
WANT_COUNT=16

# FORBIDDEN is the other half of the split: verbs that belong to the upstream
# CLI this one was extracted from and must NEVER appear here.
#
# 🔴 THIS LIST IS REDUNDANT WITH EXACT EQUALITY AND IS KEPT ANYWAY. Equality
# already refuses any of these. What the list adds is a NAMED diagnosis: a
# future reader who sees "the router half has leaked into this binary" learns
# what went wrong, where "the verb set grew by 15" only says that it did. It is
# a message, not a second gate — do not read its presence as the thing doing
# the work.
FORBIDDEN="send attention term tmux transcript view panel panels add-panel windows raise launch write query health"

# subcommands_of prints the immediate subcommand names of a command path, one
# per line, and prints nothing for a leaf.
#
# `help` is dropped by name. cobra adds it to the ROOT only, it is not a verb of
# this API, and there is no structural signal in help output to tell it from one
# — which is the price of reading the artefact instead of the tree. tree_test.go
# excludes it structurally (`RunE == nil`) rather than by name.
subcommands_of() {
  "$BIN" "$@" --help 2>/dev/null |
    awk '
      /^Available Commands:/ { inblock = 1; next }
      inblock && /^[[:space:]]*$/ { exit }
      inblock && NF > 0 { print $1 }
    ' | grep -vx 'help' || true
}

# MARKER CONTROL — can this script observe a subcommand AT ALL?
#
# 🔴 A ZERO FROM A PARSER IS NOT A MEASUREMENT UNTIL SOMETHING HAS MADE IT
# NON-ZERO. Without this, a cobra release that renamed "Available Commands:" and
# a binary with an empty command tree produce the identical empty set, and the
# failure below would blame the binary for the parser's problem.
if ! "$BIN" --help 2>/dev/null | grep -q '^Available Commands:'; then
  echo "verb-ledger: FAIL — '$BIN --help' has no 'Available Commands:' section." >&2
  echo "  Either the binary has no subcommands, or cobra's help format changed and" >&2
  echo "  this script's parser needs updating. Those are different problems; this" >&2
  echo "  message exists so they are not confused." >&2
  exit 1
fi

# Walk the tree and collect leaves.
leaves=""
walk() {
  local subs
  subs="$(subcommands_of "$@")"
  if [ -z "$subs" ]; then
    leaves="${leaves}$*
"
    return
  fi
  local s
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    walk "$@" "$s"
  done <<<"$subs"
}
while IFS= read -r top; do
  [ -n "$top" ] || continue
  walk "$top"
done <<<"$(subcommands_of)"

have="$(printf '%s' "$leaves" | sed '/^$/d' | LC_ALL=C sort)"
want="$(printf '%s\n' "$LEDGER" | LC_ALL=C sort)"

have_count="$(printf '%s\n' "$have" | sed '/^$/d' | wc -l | tr -d ' ')"
want_count="$(printf '%s\n' "$want" | sed '/^$/d' | wc -l | tr -d ' ')"

echo "verb-ledger: $BIN exposes $have_count verbs; the ledger names $want_count; the split specifies $WANT_COUNT"

rc=0

# (1) and (2): both sides pinned to the SPECIFIED number, not to each other.
if [ "$have_count" != "$WANT_COUNT" ]; then
  echo "🔴 verb-ledger: FAIL — the binary exposes $have_count verbs and the split specifies $WANT_COUNT." >&2
  rc=1
fi
if [ "$want_count" != "$WANT_COUNT" ]; then
  echo "🔴 verb-ledger: FAIL — LEDGER has $want_count rows and the split specifies $WANT_COUNT." >&2
  rc=1
fi

# (3) the two NAME SETS are equal. Reported in both directions separately,
# because "a verb was dropped" and "a verb was added" are different incidents.
missing="$(LC_ALL=C comm -13 <(printf '%s\n' "$have") <(printf '%s\n' "$want") || true)"
extra="$(LC_ALL=C comm -23 <(printf '%s\n' "$have") <(printf '%s\n' "$want") || true)"

if [ -n "$missing" ]; then
  echo "🔴 verb-ledger: FAIL — the ledger names these and the binary DOES NOT HAVE them:" >&2
  # ⚠ `sed`, NOT `printf '    %s\n' $list`. A verb path here is MULTI-WORD
  # ("agent task comment"), and an unquoted expansion word-splits it — measured:
  # a missing `task comment` printed as two separate lines, "task" and
  # "comment", which reads as two unrelated verbs. A diagnostic that misreports
  # the finding is how the wrong thing gets fixed.
  printf '%s\n' "$missing" | sed 's/^/    /' >&2
  echo "  A dropped verb breaks every caller that uses it. If the removal is" >&2
  echo "  intended, drop the LEDGER row and move WANT_COUNT in the same commit." >&2
  rc=1
fi

if [ -n "$extra" ]; then
  echo "🔴 verb-ledger: FAIL — the binary has these and the ledger DOES NOT name them:" >&2
  printf '%s\n' "$extra" | sed 's/^/    /' >&2
  rc=1
  for f in $FORBIDDEN; do
    # `(^| )<verb>$` — the forbidden name as the LAST word of a path, so it
    # matches a bare `send` and a nested `foo send` alike, and does not match a
    # verb that merely contains the word.
    if printf '%s\n' "$extra" | grep -qE "(^| )${f}\$"; then
      echo "  ⚠ '$f' belongs to the OTHER half of the split — the router surface that" >&2
      echo "    stayed with the upstream CLI. Its presence here means the extraction" >&2
      echo "    has been undone, not that a verb was added." >&2
      break
    fi
  done
fi

if [ "$rc" = 0 ]; then
  echo "verb-ledger: OK — exactly the specified set, no more and no less:"
  printf '%s\n' "$have" | sed 's/^/    /'
fi
exit "$rc"
