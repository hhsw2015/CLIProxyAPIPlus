# CPA skill family (M2-b) — one source, three skins

Generate CPA (CLIProxyAPI relay) capability skins from a single per-domain source. First batch: **image / tts / stt**
(all synchronous). Source of truth: `~/Dev/CLIProxyAPIPlus/docs/consumer-guide.md` + `frontier-aliases.json` (machine-read,
refreshed each `cpa regen`). Never push/merge (coordinator integrates). This doc lives under `skills-src/` on purpose — the
repo-root `README.md` stays the AgentHop entry page.

## The three skins

| Skin | Consumer | Produced by |
|---|---|---|
| `skills/cpa-<domain>/SKILL.md` | swarm seat | **generated** (gen-cpa-skills.py) |
| `snippets/cpa-<domain>.luau` | Carly | **generated** (gen-cpa-skills.py) |
| `cli/cpa-<domain>` | the shared backend both call (+ humans) | **hand-written** (logic in code, tested) |

One source `skills-src/<domain>.md` (a ```json spec block + prose) drives the two generated skins; the generator also
writes `cli/cpa-defaults.env` (the resolved default model per domain) bridging `frontier-aliases.json` → the hand-written
CLIs, so a `cpa regen` + re-gen refreshes every skin + the CLI default with no drift.

## Layout (paths from the repo root)
```
skills-src/{image,tts,stt}.md   # SOURCE (json spec + prose); edit here, then regenerate
scripts/gen-cpa-skills.py       # generator → SKILL.md + luau + cpa-defaults.env (idempotent)
scripts/gen-cpa-skills.test.sh  # tests (idempotence, output sanity, CLI --dry + loopback behavior, key-leak)
cli/cpa-common.sh               # shared: load keys.env + defaults, redacted --dry, opt-value guard, no-jq JSON parse
cli/{cpa-image,cpa-tts,cpa-stt} # hand-written bash + curl wrappers
cli/cpa-defaults.env            # GENERATED bridge (frontier-aliases → CLI defaults)
skills/cpa-<domain>/SKILL.md    # GENERATED ; snippets/cpa-<domain>.luau  # GENERATED
```

## Credentials — `~/.agenthop/cpa/keys.env` (0600, created; keys filled post-deploy)
Consumer-dimension keys (sk-cpa-{swarm,carly,cli}); CPA meters per key×model so domains are already separated. The CLI
wrappers use `CPA_KEY_CLI` + `CPA_BASE_URL`. **The key never appears in any skin/skill text** (even `--dry` shows the
Authorization header redacted). Empty creds ⇒ the CLIs run `--dry` only.

## Run (from the repo root)
```sh
python3 scripts/gen-cpa-skills.py          # (re)generate the two skins + cpa-defaults.env
cli/cpa-image "a red fox in snow"          # text→image (follows redirects, saves the url to a .png)
cli/cpa-tts  "你好世界" --out hi.mp3        # text→speech (raw audio bytes)
cli/cpa-stt  recording.wav                 # speech→text (prints transcript)
cli/cpa-image "x" --dry                     # preview the request without sending (key redacted)
bash scripts/gen-cpa-skills.test.sh        # the test suite
```

## Decisions (contract-confirmed)
- Generator makes **two** skins (SKILL.md + luau); the **CLI is hand-written** (logic in code, tested).
- `cpa-defaults.env` bridges the machine-readable `frontier-aliases.json` to the CLIs (regen self-refreshes).
- Keys are **consumer-dimension** (one `CPA_KEY_CLI` for all CLI domains) — CPA meters per key×model.
- First batch is **sync-only**; the async submit→poll framework (video/music/3D, `/v1/tasks/:id`) is deferred (YAGNI).
- Carly snippet cache: modules cache per VM — a regenerated snippet is picked up only on a **VM reset** (noted in each `.luau`).

## Robustness (r2, codex:Work review 3b48d60)
- Option values are validated (a missing/flag-occupied value like `--model --dry` is rejected, never swallowed into a
  real POST); `--` truly terminates option parsing.
- `cpa-stt` requires the response to be `{text: <string>}`; a missing/wrong-typed `text` fails non-zero and does NOT
  overwrite the prior transcript (`{"text":""}` is a legit empty result).
- `cpa-stt` quotes the multipart filename for curl (handles `,`/`;` in paths).
- `cpa-image` follows redirects and fails on an empty download (no 0-byte "success").
