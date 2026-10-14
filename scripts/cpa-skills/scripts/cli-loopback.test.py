#!/usr/bin/env python3
"""
cli-loopback.test.py — REAL-behavior tests for the cpa-<domain> CLIs (M2-b r2). Spins a loopback HTTP server that
records requests, points the CLIs at it with a FAKE key, and asserts the terminal states the codex:Work review (3b48d60)
flagged — behaviors a --dry string check cannot catch:

  CPA-ARGS        a missing/flag-occupied option value (`--model --dry`) is REJECTED → ZERO requests (no real POST)
  CPA-STT-SCHEMA  a bad/absent `text` ({}/null/123) fails non-zero and does NOT overwrite the prior transcript; {"text":""} ok
  CPA-STT-PATH    a filename with ',' / ';' uploads its real bytes (curl second-level parse handled)
  CPA-IMAGE-REDIRECT  a 302 data url is followed to the real asset (non-empty); an empty/redirect-to-empty body is an error

No CPA/upstream calls — only this local server + a fake key + synthetic inputs.
"""
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading

REPO = pathlib.Path(__file__).resolve().parent.parent
CLI = REPO / "cli"
REQS = []  # (path, body bytes)

PNG = b"\x89PNG\r\n\x1a\nREALIMAGEBYTES"


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):  # quiet
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(b)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        body = self.rfile.read(n) if n else b""
        REQS.append((self.path, body))
        host = f"http://{self.server.server_address[0]}:{self.server.server_address[1]}"
        if self.path == "/v1/images/generations":
            url = host + ("/empty-img" if b'"emptyimg"' in body else "/redir-img")
            self._json({"data": [{"url": url}]})
        elif self.path == "/v1/audio/speech":
            self.send_response(200); self.send_header("Content-Type", "audio/mpeg"); self.end_headers(); self.wfile.write(b"\xff\xf3AUDIO")
        elif self.path == "/v1/audio/transcriptions":
            if b"badshape" in body:
                self._json({})                 # 200 but no text field
            elif b"emptytext" in body:
                self._json({"text": ""})       # legit empty
            else:
                self._json({"text": "hello world"})
        else:
            self.send_response(404); self.end_headers()

    def do_GET(self):
        host = f"http://{self.server.server_address[0]}:{self.server.server_address[1]}"
        if self.path == "/redir-img":
            self.send_response(302); self.send_header("Location", host + "/real-img.png"); self.end_headers()
        elif self.path == "/real-img.png":
            self.send_response(200); self.send_header("Content-Type", "image/png"); self.end_headers(); self.wfile.write(PNG)
        elif self.path == "/empty-img":
            self.send_response(200); self.end_headers()  # 0 bytes
        else:
            self.send_response(404); self.end_headers()


def main() -> int:
    srv = http.server.HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    tmp = tempfile.mkdtemp(prefix="cpa-loopback-")
    keys = os.path.join(tmp, "keys.env")
    with open(keys, "w") as f:
        f.write(f"CPA_BASE_URL={base}\nCPA_KEY_CLI=sk-fake-loopback\n")
    os.chmod(keys, 0o600)
    env = {**os.environ, "CPA_KEYS_ENV": keys}

    passed = failed = 0

    def check(name, cond):
        nonlocal passed, failed
        print(("ok  " if cond else "FAIL ") + name)
        passed += cond
        failed += (not cond)

    def run(args):
        return subprocess.run([str(CLI / args[0]), *args[1:]], env=env, capture_output=True, text=True, cwd=tmp)

    # ── CPA-ARGS: `--model --dry` must be REJECTED → zero requests (not a real POST) ─────────────────────────────────
    REQS.clear()
    r = run(["cpa-image", "a cat", "--model", "--dry"])
    check("args: cpa-image '--model --dry' rejected (nonzero)", r.returncode != 0)
    check("args: cpa-image '--model --dry' sent ZERO requests", len(REQS) == 0)
    REQS.clear()
    r = run(["cpa-tts", "hello", "--voice", "--dry"])
    check("args: cpa-tts '--voice --dry' rejected + zero requests", r.returncode != 0 and len(REQS) == 0)
    # positive: a proper --dry sends nothing and exits 0
    REQS.clear()
    r = run(["cpa-image", "a cat", "--dry"])
    check("args: proper --dry = zero requests, exit 0", r.returncode == 0 and len(REQS) == 0)
    # `--` truly terminates option parsing: a prompt starting with '-' after `--` is accepted
    REQS.clear()
    r = run(["cpa-image", "--dry", "--", "-weird prompt"])
    check("args: `--` terminates options (leading-dash prompt ok)", r.returncode == 0)

    # ── CPA-STT-SCHEMA: bad text fails + does NOT overwrite; {"text":""} ok ──────────────────────────────────────────
    aud = os.path.join(tmp, "a.wav"); open(aud, "wb").write(b"RIFFfakeWAV")
    prior = os.path.join(tmp, "prior.txt"); open(prior, "w").write("PRIOR-TRANSCRIPT")
    r = run(["cpa-stt", aud, "--model", "badshape", "--out", prior])
    check("stt: bad-shape {} fails nonzero", r.returncode != 0)
    check("stt: prior transcript NOT overwritten on bad response", open(prior).read() == "PRIOR-TRANSCRIPT")
    r = run(["cpa-stt", aud, "--model", "emptytext"])
    check("stt: {\"text\":\"\"} is a legit empty result (exit 0)", r.returncode == 0 and r.stdout.strip() == "")
    r = run(["cpa-stt", aud, "--model", "good"])
    check("stt: normal transcript printed", r.returncode == 0 and "hello world" in r.stdout)

    # ── CPA-STT-PATH: special filenames upload the CORRECT bytes (not another file / not an error) ───────────────────
    weird = os.path.join(tmp, "audio,part.wav"); open(weird, "wb").write(b"COMMA-FILE-BYTES")
    REQS.clear()
    r = run(["cpa-stt", weird, "--model", "good"])
    got = any(b"COMMA-FILE-BYTES" in body for (p, body) in REQS if p == "/v1/audio/transcriptions")
    check("stt: comma-filename uploads its real bytes", r.returncode == 0 and got)
    # backslash+quote in the name: must upload ITS bytes, not a decoy resolved by curl's escape parsing
    bsl = os.path.join(tmp, 'audio"x.wav'); open(bsl, "wb").write(b"BACKSLASH-TARGET-BYTES")
    open(os.path.join(tmp, "audiox.wav"), "wb").write(b"DECOY-BYTES")  # a file curl escaping might resolve to instead
    REQS.clear()
    r = run(["cpa-stt", bsl, "--model", "good"])
    got_t = any(b"BACKSLASH-TARGET-BYTES" in body for (p, body) in REQS if p == "/v1/audio/transcriptions")
    got_decoy = any(b"DECOY-BYTES" in body for (p, body) in REQS if p == "/v1/audio/transcriptions")
    check("stt: quote-in-name uploads TARGET bytes (not a decoy)", r.returncode == 0 and got_t and not got_decoy)
    # single-file guard: a second file (positional or after --) must be rejected, never silently replace the first
    r = run(["cpa-stt", weird, "--", os.path.join(tmp, "a.wav")])
    check("stt: second file after -- rejected (one file only)", r.returncode != 0)
    r = run(["cpa-stt", os.path.join(tmp, "a.wav"), weird])
    check("stt: two positional files rejected", r.returncode != 0)
    # the TWO entrances the r3 residual leaked through: the inherited TMPDIR path, and the reattached extension.
    # (a) a ';' in TMPDIR must not let curl read a decoy — the server must get the REQUESTED bytes.
    semidir = os.path.join(tmp, "cache;semi"); os.makedirs(semidir, exist_ok=True)
    open(os.path.join(tmp, "cache"), "wb").write(b"DECOY-TMPDIR-BYTES")   # where a ';' split could resolve
    src = os.path.join(tmp, "req.wav"); open(src, "wb").write(b"REQUESTED-TMPDIR-BYTES")
    REQS.clear()
    r = subprocess.run([str(CLI / "cpa-stt"), src, "--model", "good"], env={**env, "TMPDIR": semidir}, capture_output=True, text=True, cwd=tmp)
    got_req = any(b"REQUESTED-TMPDIR-BYTES" in body for (p, body) in REQS if p == "/v1/audio/transcriptions")
    got_decoy = any(b"DECOY-TMPDIR-BYTES" in body for (p, body) in REQS)
    check("stt: ';'-in-TMPDIR uploads REQUESTED bytes (not decoy)", r.returncode == 0 and got_req and not got_decoy)
    # (b) a comma in the source extension must not leak into curl's form grammar.
    commaext = os.path.join(tmp, "take.wav,part"); open(commaext, "wb").write(b"COMMAEXT-BYTES")
    REQS.clear()
    r = run(["cpa-stt", commaext, "--model", "good"])
    check("stt: comma-in-extension source uploads correct bytes", r.returncode == 0 and any(b"COMMAEXT-BYTES" in body for (p, body) in REQS if p == "/v1/audio/transcriptions"))

    # ── CPA-ARGS: an option value with a trailing newline is kept verbatim (not stripped by a $() capture) ───────────
    weird_out = "o\n"  # cpa-tts writes to --out; the value must survive the validator unchanged
    r = run(["cpa-tts", "hi", "--out", weird_out])
    check("args: --out trailing-LF value preserved (writes to 'o\\n', not 'o')",
          r.returncode == 0 and os.path.exists(os.path.join(tmp, "o\n")) and not os.path.exists(os.path.join(tmp, "o")))

    # ── CPA-IMAGE-REDIRECT: 302 followed to the real asset (non-empty); empty body = error ──────────────────────────
    r = run(["cpa-image", "a cat", "--out", os.path.join(tmp, "img.png")])
    out_png = os.path.join(tmp, "img.png")
    check("image: 302 redirect followed → non-empty file", r.returncode == 0 and os.path.exists(out_png) and os.path.getsize(out_png) == len(PNG))
    r = run(["cpa-image", "a cat", "--model", "emptyimg", "--out", os.path.join(tmp, "empty.png")])
    check("image: empty download is an ERROR (not 0-byte success)", r.returncode != 0 and not os.path.exists(os.path.join(tmp, "empty.png")))
    # image default size comes from the alias (media[Image generation].size) via cpa-defaults.env
    REQS.clear()
    r = run(["cpa-image", "a cat", "--out", os.path.join(tmp, "i2.png")])
    sized = any(b'"size": "2048x2048"' in body for (p, body) in REQS if p == "/v1/images/generations")
    check("image: default size from alias (2048x2048)", r.returncode == 0 and sized)

    srv.shutdown()
    print(f"\nPASS={passed} FAIL={failed}")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
