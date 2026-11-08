#!/usr/bin/env python3
"""Make the signed values the smoke test needs, from the shared secrets.

    mint.py identity SECRET CUSTOMER_ID          # the header the gateway adds for the Order and Cart services
    mint.py session  SECRET CUSTOMER_ID [admin]  # a signed-in gateway cookie value, as if the person had logged in

This is test tooling: it signs with the demo secrets, exactly as the gateway
does after a real OIDC login. The formats are in
web-service/internal/identity and web-service/internal/session.
"""
import base64, hashlib, hmac, json, sys, time

def b64(b): return base64.urlsafe_b64encode(b).rstrip(b"=").decode()
def sign(secret, msg): return b64(hmac.new(secret.encode(), msg, hashlib.sha256).digest())

def identity(secret, cust):
    body = "v1." + b64(json.dumps({"sub": cust, "exp": int(time.time()) + 300}, separators=(",", ":")).encode())
    return body + "." + sign(secret, body.encode())

def session(secret, cust, admin):
    v = {"c": cust, "e": cust + "@smoke.test", "n": "Smoke", "x": int(time.time()) + 300}
    if admin:
        v["a"] = True
    p = b64(json.dumps(v, separators=(",", ":")).encode())
    return p + "." + b64(hmac.new(secret.encode(), b"session/v1\0" + p.encode(), hashlib.sha256).digest())

if __name__ == "__main__":
    kind, secret, cust, *rest = sys.argv[1:]
    print(identity(secret, cust) if kind == "identity" else session(secret, cust, bool(rest)))
