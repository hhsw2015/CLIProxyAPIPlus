<!-- CPA skill family — SOURCE OF TRUTH for the `image` domain (M2-b). The JSON block below is machine-read by
scripts/gen-cpa-skills.py to emit the two generated skins (skills/cpa-image/SKILL.md + snippets/cpa-image.luau) and the
CLI default (cli/cpa-defaults.env). The prose after it is copied verbatim into SKILL.md's body. Default MODEL is NOT
hard-coded here — the generator resolves it from ~/Dev/CLIProxyAPIPlus/docs/frontier-aliases.json (media[default_model_key].id)
at generation time, so each `cpa regen` + re-gen refreshes the strongest id. Edit this file, then re-run the generator. -->

```json
{
  "domain": "image",
  "cli": "cpa-image",
  "skill_name": "cpa-image",
  "description": "Generate images through CPA (CLIProxyAPI relay). Use when the user asks to 生成图片 / 文生图 / make an image / text-to-image via the swarm's CPA endpoint. Runs cli/cpa-image (sync /v1/images/generations).",
  "argument_hint": "<image prompt> [--size 1024x1024] [--n 1] [--model <id>] [--out <path>]",
  "endpoint": "/v1/images/generations",
  "method": "POST",
  "mode": "sync",
  "output": "url",
  "default_model_key": "Image generation",
  "params": [
    {"name": "prompt", "required": true, "desc": "the image description (positional)"},
    {"name": "--size", "required": false, "default": "(model config)", "desc": "WxH, e.g. 1024x1024; default is the model-config size from frontier-aliases"},
    {"name": "--n", "required": false, "default": "1", "desc": "number of images"},
    {"name": "--model", "required": false, "desc": "override the default model id"},
    {"name": "--out", "required": false, "desc": "output file path (default: cwd timestamped .png)"}
  ],
  "gotchas": [
    "size is WxH like 1024x1024; CPA handles the DashScope/fal/Azure routing internally",
    "response is sync JSON data[].url — the CLI downloads the first url to --out (urls expire, fetch promptly)",
    "a listed model id is not a health guarantee; on a transient failure retry the same id (CPA fill-first fails over)"
  ]
}
```

## What it does

`cpa-image` generates an image from a text prompt via CPA's OpenAI-wire `POST /v1/images/generations` (synchronous:
the response carries `data[].url`). The CLI downloads the first returned URL to a local file and prints the path.

## Request shape

```
POST {CPA}/v1/images/generations
Authorization: Bearer <CPA_KEY_CLI>
{ "model": "<default from frontier-aliases>", "prompt": "<prompt>", "size": "<model-config default>", "n": 1 }
→ { "data": [ { "url": "https://…" } ] }
```

## Notes

- This is **image generation**. Image *editing* (`/v1/images/edits`) is a separate domain for a later batch.
- The default model is the current strongest `Image generation` id from `frontier-aliases.json`; override with `--model`.
- No async polling — image generation is synchronous. (Async media domains — video/music/3D — poll `/v1/tasks/:id`.)
