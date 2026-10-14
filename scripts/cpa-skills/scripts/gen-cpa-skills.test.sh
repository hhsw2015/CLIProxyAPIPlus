#!/usr/bin/env bash
# Test harness for the CPA skill family (M2-b): generator idempotence + output sanity + CLI --dry request shapes +
# key-leak guard. Run: bash scripts/gen-cpa-skills.test.sh   (needs python3 + the frontier-aliases.json source)
# NB: no `pipefail` — assertions pipe CLI output into `grep -q`, which exits on first match and SIGPIPEs the producer;
# with pipefail that would flakily mark the pipeline failed. We want grep's exit status, so pipefail stays OFF.
set -u
cd "$(cd "$(dirname "$0")" && pwd)/.."   # repo root
pass=0; fail=0
t() { if eval "$2" >/dev/null 2>&1; then echo "ok  $1"; pass=$((pass+1)); else echo "FAIL $1"; fail=$((fail+1)); fi; }

# ── generator: runs + idempotent (two runs byte-identical) ──────────────────────────────────────────────────────────
t "generator runs clean" "python3 scripts/gen-cpa-skills.py"
hash_outputs() { find skills snippets cli/cpa-defaults.env -type f 2>/dev/null | sort | xargs shasum -a256 | shasum -a256 | cut -d' ' -f1; }
H1=$(hash_outputs); python3 scripts/gen-cpa-skills.py >/dev/null 2>&1; H2=$(hash_outputs)
t "generator idempotent (two runs byte-identical)" "[ '$H1' = '$H2' ]"

# ── output sanity: 3 domains, each skin generated with the resolved default + don't-edit marker ──────────────────────
for d in image tts stt; do
  t "skills/cpa-$d/SKILL.md generated" "[ -f skills/cpa-$d/SKILL.md ]"
  t "SKILL.md $d has name frontmatter" "grep -q '^name: cpa-$d' skills/cpa-$d/SKILL.md"
  t "SKILL.md $d marked GENERATED" "grep -q 'GENERATED from skills-src' skills/cpa-$d/SKILL.md"
  t "snippets/cpa-$d.luau returns a function" "grep -q 'return function' snippets/cpa-$d.luau"
  t "snippet $d calls tern.process.run" "grep -q 'tern.process.run(argv)' snippets/cpa-$d.luau"
  t "snippet $d documents VM-reset cache" "grep -q 'VM RESET' snippets/cpa-$d.luau"
done
t "cpa-defaults.env has image model" "grep -q '^CPA_DEFAULT_MODEL_IMAGE=' cli/cpa-defaults.env"
t "cpa-defaults.env has tts voice" "grep -q '^CPA_DEFAULT_VOICE_TTS=' cli/cpa-defaults.env"

# ── CLI --dry: correct endpoint + redacted key, no JSON-body for multipart stt ──────────────────────────────────────
t "cpa-image --dry hits /v1/images/generations" "cli/cpa-image 'a cat' --dry | grep -q 'POST /v1/images/generations'"
t "cpa-image --dry redacts the key" "cli/cpa-image 'a cat' --dry | grep -qF 'Bearer ***redacted'"
t "cpa-tts --dry hits /v1/audio/speech" "cli/cpa-tts 'hi' --dry | grep -q 'POST /v1/audio/speech'"
t "cpa-tts --dry notes raw-audio output" "cli/cpa-tts 'hi' --dry | grep -q 'RAW AUDIO BYTES'"
t "cpa-stt --dry hits /v1/audio/transcriptions multipart" "cli/cpa-stt /tmp/a.wav --dry | grep -q 'multipart/form-data'"
t "cpa-stt --dry shows -F file=@" "cli/cpa-stt /tmp/a.wav --dry | grep -q -- '-F file=@/tmp/a.wav'"

# ── key-leak guard: a real key value must NEVER appear in --dry output ───────────────────────────────────────────────
TK=/tmp/cpa-test-keys.$$.env; printf 'CPA_BASE_URL=https://cpa.example\nCPA_KEY_CLI=sk-LEAKCHECK-TOKEN\n' > "$TK"; chmod 600 "$TK"
LEAK=$(CPA_KEYS_ENV="$TK" cli/cpa-image x --dry; CPA_KEYS_ENV="$TK" cli/cpa-tts y --dry; CPA_KEYS_ENV="$TK" cli/cpa-stt /tmp/a.wav --dry)
t "no key leak in --dry output" "! printf '%s' \"\$LEAK\" | grep -q 'sk-LEAKCHECK-TOKEN'"
rm -f "$TK"

# ── usage errors fail closed ────────────────────────────────────────────────────────────────────────────────────────
t "cpa-image with no prompt errors" "! cli/cpa-image"
t "cpa-image unknown flag errors" "! cli/cpa-image x --bogus"
t "cpa-stt real call on missing creds fails closed (no --dry)" "! CPA_KEYS_ENV=/dev/null cli/cpa-stt /tmp/a.wav"

# ── CPA-IMAGE-DEFAULT-DOC gate (r5): generated default-size doc == defaults.env, and follows an alias change ──────────
echo "--- size-doc gate (scripts/check-size-doc.py) ---"
if python3 scripts/check-size-doc.py; then echo "ok  size-doc gate"; pass=$((pass+1)); else echo "FAIL size-doc gate"; fail=$((fail+1)); fi

# ── real CLI behavior (loopback HTTP) — the r2 review fixes (args/stt-schema/stt-path/image-redirect) ─────────────────
echo "--- loopback integration (scripts/cli-loopback.test.py) ---"
if python3 scripts/cli-loopback.test.py; then echo "ok  loopback integration suite"; pass=$((pass+1)); else echo "FAIL loopback integration suite"; fail=$((fail+1)); fi  # r6: this else had dropped the counter → false-green; restored

echo
echo "PASS=$pass FAIL=$fail"
[ "$fail" = 0 ] && echo "ALL PASS" || exit 1
