#!/usr/bin/env python3
"""Diff two CPA config.yaml files and report what changed.

Answers "which configs changed" between two generated configs (e.g. the previous
deploy vs a fresh regen from the latest SKYROUTER): client-callable models
added/removed, provider channels (entries) added/removed, per-model provider-count
changes, and per-entry priority / base-url / disabled / model-set changes.

Usage:
    config_diff.py OLD.yaml NEW.yaml           # human report
    config_diff.py OLD.yaml NEW.yaml --json     # machine-readable JSON
    config_diff.py OLD.yaml NEW.yaml --json out.json  # also write JSON to file
"""
import sys
import json
import yaml

SECTIONS = ("claude-api-key", "gemini-api-key", "openai-compatibility")


def _client_model(m):
    """The client-facing id for a model entry: alias if set, else name."""
    if not isinstance(m, dict):
        return str(m)
    a = (m.get("alias") or "").strip()
    return a if a else (m.get("name") or "").strip()


def inventory(path):
    """Parse a CPA config into {entries, model_providers}.

    entries: name -> {priority, base_url, disabled, section, models:set(client id)}
    model_providers: client model -> set(entry names) (only enabled entries)
    """
    with open(path) as f:
        cfg = yaml.safe_load(f) or {}
    entries = {}
    model_providers = {}
    for section in SECTIONS:
        for e in (cfg.get(section) or []):
            if not isinstance(e, dict):
                continue
            name = (e.get("name") or "").strip() or "(unnamed)"
            disabled = bool(e.get("disabled"))
            models = set()
            for m in (e.get("models") or []):
                cm = _client_model(m)
                if cm:
                    models.add(cm)
            entries[name] = {
                "priority": e.get("priority"),
                "base_url": (e.get("base-url") or "").strip(),
                "disabled": disabled,
                "section": section,
                "models": models,
            }
            if not disabled:
                for cm in models:
                    model_providers.setdefault(cm, set()).add(name)
    return entries, model_providers


def diff(old_path, new_path):
    old_e, old_mp = inventory(old_path)
    new_e, new_mp = inventory(new_path)

    old_models, new_models = set(old_mp), set(new_mp)
    added_models = sorted(new_models - old_models)
    removed_models = sorted(old_models - new_models)

    old_names, new_names = set(old_e), set(new_e)
    added_entries = sorted(new_names - old_names)
    removed_entries = sorted(old_names - new_names)

    # per-entry changes for surviving entries
    changed_entries = {}
    for name in sorted(old_names & new_names):
        o, n = old_e[name], new_e[name]
        ch = {}
        if o["priority"] != n["priority"]:
            ch["priority"] = [o["priority"], n["priority"]]
        if o["base_url"] != n["base_url"]:
            ch["base_url"] = [o["base_url"], n["base_url"]]
        if o["disabled"] != n["disabled"]:
            ch["disabled"] = [o["disabled"], n["disabled"]]
        m_add = sorted(n["models"] - o["models"])
        m_del = sorted(o["models"] - n["models"])
        if m_add:
            ch["models_added"] = m_add
        if m_del:
            ch["models_removed"] = m_del
        if ch:
            changed_entries[name] = ch

    # per-model provider-count delta (surviving models only)
    provider_delta = {}
    for m in sorted(new_models & old_models):
        o, n = len(old_mp[m]), len(new_mp[m])
        if o != n:
            provider_delta[m] = [o, n]

    return {
        "summary": {
            "models_old": len(old_models),
            "models_new": len(new_models),
            "models_added": len(added_models),
            "models_removed": len(removed_models),
            "entries_old": len(old_names),
            "entries_new": len(new_names),
            "entries_added": len(added_entries),
            "entries_removed": len(removed_entries),
            "entries_changed": len(changed_entries),
        },
        "models_added": added_models,
        "models_removed": removed_models,
        "entries_added": added_entries,
        "entries_removed": removed_entries,
        "entries_changed": changed_entries,
        "provider_count_delta": provider_delta,
    }


def render(d):
    s = d["summary"]
    out = []
    out.append("=" * 64)
    out.append("CONFIG DIFF")
    out.append("=" * 64)
    out.append(
        f"models:  {s['models_old']} -> {s['models_new']}  "
        f"(+{s['models_added']} / -{s['models_removed']})"
    )
    out.append(
        f"channels:{s['entries_old']} -> {s['entries_new']}  "
        f"(+{s['entries_added']} / -{s['entries_removed']}, ~{s['entries_changed']} changed)"
    )
    if d["models_added"]:
        out.append("\n+ NEW client models (" + str(len(d["models_added"])) + "):")
        for m in d["models_added"]:
            out.append(f"    + {m}")
    if d["models_removed"]:
        out.append("\n- REMOVED client models (" + str(len(d["models_removed"])) + "):")
        for m in d["models_removed"]:
            out.append(f"    - {m}")
    if d["entries_added"]:
        out.append("\n+ NEW channels (" + str(len(d["entries_added"])) + "):")
        for e in d["entries_added"]:
            out.append(f"    + {e}")
    if d["entries_removed"]:
        out.append("\n- REMOVED channels (" + str(len(d["entries_removed"])) + "):")
        for e in d["entries_removed"]:
            out.append(f"    - {e}")
    if d["entries_changed"]:
        out.append("\n~ CHANGED channels (" + str(len(d["entries_changed"])) + "):")
        for name, ch in d["entries_changed"].items():
            parts = []
            if "priority" in ch:
                parts.append(f"prio {ch['priority'][0]}->{ch['priority'][1]}")
            if "disabled" in ch:
                parts.append(f"disabled {ch['disabled'][0]}->{ch['disabled'][1]}")
            if "base_url" in ch:
                parts.append("base-url changed")
            if "models_added" in ch:
                parts.append(f"+{len(ch['models_added'])} models")
            if "models_removed" in ch:
                parts.append(f"-{len(ch['models_removed'])} models")
            out.append(f"    ~ {name}: {', '.join(parts)}")
    if d["provider_count_delta"]:
        out.append(
            "\n# provider-count changes (model: old->new providers, "
            + str(len(d["provider_count_delta"]))
            + "):"
        )
        for m, (o, n) in d["provider_count_delta"].items():
            arrow = "up" if n > o else "down"
            out.append(f"    {m}: {o}->{n} ({arrow})")
    if not (
        d["models_added"]
        or d["models_removed"]
        or d["entries_added"]
        or d["entries_removed"]
        or d["entries_changed"]
    ):
        out.append("\n(no changes)")
    out.append("=" * 64)
    return "\n".join(out)


def main():
    args = [a for a in sys.argv[1:]]
    as_json = "--json" in args
    if as_json:
        args.remove("--json")
    json_out = None
    if as_json and len(args) >= 3:
        json_out = args[2]
    if len(args) < 2:
        print("usage: config_diff.py OLD.yaml NEW.yaml [--json [out.json]]", file=sys.stderr)
        sys.exit(2)
    d = diff(args[0], args[1])
    if as_json:
        text = json.dumps(d, indent=2, ensure_ascii=False)
        if json_out:
            with open(json_out, "w") as f:
                f.write(text)
            print(f"wrote {json_out}")
        else:
            print(text)
    else:
        print(render(d))


if __name__ == "__main__":
    main()
