#!/usr/bin/env python3
"""Reference minter for CPA ephemeral capability tokens (cpa-eph HS256 JWT).

Testing/reference only — the production minter lives on the dispatcher. Shares
CPA_EPH_SECRET with the CPA verifier (internal/access/config_access/provider.go:verifyEphToken).

  CPA_EPH_SECRET=... python3 cpa_mint_eph.py [--ttl 3600] [--sub agent-id]

Prints a token to use as the bearer against CPA (ANTHROPIC_AUTH_TOKEN / OPENAI_API_KEY).
See docs/cpa-ephemeral-token.md.
"""
import argparse, base64, hashlib, hmac, json, os, sys, time


def b64(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--ttl", type=int, default=3600, help="lifetime in seconds (<=3600 = 60min box life)")
    ap.add_argument("--sub", default="test", help="agent / launch id (audit only)")
    args = ap.parse_args()

    secret = os.environ.get("CPA_EPH_SECRET", "").encode()
    if not secret:
        sys.exit("set CPA_EPH_SECRET (same secret the CPA host verifies with)")

    now = int(time.time())
    header = {"alg": "HS256", "typ": "JWT"}
    payload = {"iss": "cpa-eph", "iat": now, "exp": now + args.ttl, "sub": args.sub}
    signing = (
        b64(json.dumps(header, separators=(",", ":")).encode())
        + "."
        + b64(json.dumps(payload, separators=(",", ":")).encode())
    )
    sig = b64(hmac.new(secret, signing.encode(), hashlib.sha256).digest())
    print(signing + "." + sig)


if __name__ == "__main__":
    main()
