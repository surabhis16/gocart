#!/usr/bin/env python3
"""Validate infra/k8s without a cluster.

Checks every manifest against the real Kubernetes API schemas (strictly:
unknown fields are errors), then the things a schema cannot catch: selectors
that match their pods, probes that name real ports, Services that point at a
workload, kustomize overlays whose references resolve, and image tags that
agree with the VERSION file.

    pip install kubernetes-validate pyyaml
    python3 scripts/check-k8s.py
"""
import sys, pathlib, yaml, kubernetes_validate

ROOT = pathlib.Path(__file__).resolve().parent.parent / "infra" / "k8s"
VERSION = "1.31"
problems = []

def docs(path):
    return [d for d in yaml.safe_load_all(path.read_text()) if d]

# 1. Every plain manifest validates against the real Kubernetes schemas.
manifests = [ROOT/"base/product-service.yaml", ROOT/"base/order-service.yaml",
             ROOT/"base/payment-service.yaml", ROOT/"base/cart-service.yaml", ROOT/"base/web-service.yaml",
             ROOT/"base/dex.yaml", ROOT/"base/networkpolicy.yaml",              ROOT/"cassandra/cassandra.yaml", ROOT/"overlays/local/namespace.yaml",
             ROOT/"overlays/staging/namespace.yaml"]
n = 0
for m in manifests:
    for d in docs(m):
        n += 1
        try:
            kubernetes_validate.validate(d, VERSION, strict=True)
        except Exception as e:
            problems.append(f"{m.relative_to(ROOT)}: {d.get('kind')}/{d['metadata']['name']}: {e}")
print(f"schema-validated {n} objects against Kubernetes {VERSION} (strict: unknown fields are errors)")

# 2. Things schemas cannot catch.
by_kind = {}
for m in manifests:
    for d in docs(m):
        by_kind.setdefault((d["kind"], d["metadata"]["name"]), d)

for (kind, name), d in by_kind.items():
    if kind not in ("Deployment", "StatefulSet"):
        continue
    sel = d["spec"]["selector"]["matchLabels"]
    lab = d["spec"]["template"]["metadata"]["labels"]
    if any(lab.get(k) != v for k, v in sel.items()):
        problems.append(f"{kind}/{name}: selector {sel} does not match pod labels {lab}")
    pod = d["spec"]["template"]["spec"]
    for c in pod["containers"]:
        ports = {p["name"] for p in c.get("ports", [])}
        for probe in ("readinessProbe", "livenessProbe"):
            p = c.get(probe, {}).get("httpGet")
            if p and p["port"] not in ports:
                problems.append(f"{kind}/{name}: {probe} uses port {p['port']!r}, container declares {sorted(ports)}")
        mounts = {v["mountPath"]: v["name"] for v in c.get("volumeMounts", [])}
        declared = {v["name"] for v in pod.get("volumes", [])} | {t["metadata"]["name"] for t in d["spec"].get("volumeClaimTemplates", [])}
        for mp, vn in mounts.items():
            if vn not in declared:
                problems.append(f"{kind}/{name}: mounts volume {vn!r} that is not declared")
        if kind == "Deployment" and "limits" not in c.get("resources", {}):
            problems.append(f"{kind}/{name}: no resource limits")
    for v in pod.get("volumes", []):
        claim = v.get("persistentVolumeClaim", {}).get("claimName")
        if claim and ("PersistentVolumeClaim", claim) not in by_kind:
            problems.append(f"{kind}/{name}: references missing PVC {claim!r}")

for (kind, name), d in by_kind.items():
    if kind != "Service" or d["spec"].get("clusterIP") == "None" and False:
        continue
    sel = d["spec"]["selector"]
    targets = [x for (k, _), x in by_kind.items() if k in ("Deployment", "StatefulSet")
               and all(x["spec"]["template"]["metadata"]["labels"].get(a) == b for a, b in sel.items())]
    if not targets:
        problems.append(f"Service/{name}: selector {sel} matches no workload")
    for t in targets:
        ports = {p["name"] for c in t["spec"]["template"]["spec"]["containers"] for p in c.get("ports", [])}
        for sp in d["spec"]["ports"]:
            tp = sp.get("targetPort", sp["port"])
            if isinstance(tp, str) and tp not in ports:
                problems.append(f"Service/{name}: targetPort {tp!r} not exposed by {t['metadata']['name']}")

# the Order service must be able to find the Product service by its Service name
order = by_kind[("Deployment", "order-service")]["spec"]["template"]["spec"]["containers"][0]
env = {e["name"]: e.get("value") for e in order["env"]}
for var, want_svc, want_port in (("PRODUCT_URL", "product-service", 8080), ("PAYMENT_URL", "payment-service", 8082)):
    if env.get(var) != f"http://{want_svc}:{want_port}":
        problems.append(f"order-service {var} is {env.get(var)}, expected http://{want_svc}:{want_port}")
    port = by_kind[("Service", want_svc)]["spec"]["ports"][0]["port"]
    if port != want_port:
        problems.append(f"{want_svc} Service port is {port}, order-service calls {want_port}")

# the Cart service must find both services it calls, and the Order service must
# be a Service it can reach
cartc = by_kind[("Deployment", "cart-service")]["spec"]["template"]["spec"]["containers"][0]
cenv = {e["name"]: e.get("value") for e in cartc["env"]}
for var, want_svc, want_port in (("PRODUCT_URL", "product-service", 8080), ("ORDER_URL", "order-service", 8081)):
    if cenv.get(var) != f"http://{want_svc}:{want_port}":
        problems.append(f"cart-service {var} is {cenv.get(var)}, expected http://{want_svc}:{want_port}")
cport = by_kind[("Service", "cart-service")]["spec"]["ports"][0]["port"]
if cport != 8083 or cenv.get("PORT") != "8083":
    problems.append(f"cart-service must listen on 8083 (Service {cport}, PORT {cenv.get('PORT')})")

# the web gateway must find all four services it forwards to
webc = by_kind[("Deployment", "web-service")]["spec"]["template"]["spec"]["containers"][0]
wenv = {e["name"]: e.get("value") for e in webc["env"]}
for var, want_svc, want_port in (("PRODUCT_URL", "product-service", 8080), ("ORDER_URL", "order-service", 8081),
                                 ("PAYMENT_URL", "payment-service", 8082), ("CART_URL", "cart-service", 8083)):
    if wenv.get(var) != f"http://{want_svc}:{want_port}":
        problems.append(f"web-service {var} is {wenv.get(var)}, expected http://{want_svc}:{want_port}")
wport = by_kind[("Service", "web-service")]["spec"]["ports"][0]["port"]
if wport != 3000 or wenv.get("PORT") != "3000":
    problems.append(f"web-service must listen on 3000 (Service {wport}, PORT {wenv.get('PORT')})")

# Every secret a workload reads must be produced by every overlay that deploys it.
needed = set()
for (kd, _), d in by_kind.items():
    if kd == "Deployment":
        for c in d["spec"]["template"]["spec"]["containers"]:
            for e in c.get("env", []):
                ref = e.get("valueFrom", {}).get("secretKeyRef")
                if ref and not ref.get("optional"):
                    needed.add((ref["name"], ref["key"]))
for ov in ("overlays/local", "overlays/staging"):
    gen = {}
    for g in yaml.safe_load((ROOT/ov/"kustomization.yaml").read_text()).get("secretGenerator", []):
        for lit in g.get("literals", []):
            gen[(g["name"], lit.split("=", 1)[0])] = True
    for name, key in needed:
        if (name, key) not in gen:
            problems.append(f"{ov} does not generate secret {name}/{key}, which a Deployment reads")

# Every ConfigMap key a workload reads must come from a generator in the base.
base_k = yaml.safe_load((ROOT/"base/kustomization.yaml").read_text())
cm_keys = {}
for g in base_k.get("configMapGenerator", []):
    cm_keys[g["name"]] = {l.split("=", 1)[0] for l in g.get("literals", [])} | {f.split("=", 1)[0] for f in g.get("files", [])}
    for f in g.get("files", []):
        if not (ROOT/"base"/f.split("=", 1)[-1]).exists():
            problems.append(f"base: configMapGenerator {g['name']} reads {f}, which does not exist")
for (kd, nm), d in by_kind.items():
    if kd == "Deployment":
        pod = d["spec"]["template"]["spec"]
        for c in pod["containers"]:
            for e in c.get("env", []):
                ref = e.get("valueFrom", {}).get("configMapKeyRef")
                if ref and ref["key"] not in cm_keys.get(ref["name"], ()):
                    problems.append(f"{nm}: reads ConfigMap {ref['name']}/{ref['key']}, which the base does not generate")
        for v in pod.get("volumes", []):
            cm = v.get("configMap", {}).get("name")
            if cm and cm not in cm_keys:
                problems.append(f"{nm}: mounts ConfigMap {cm!r}, which the base does not generate")

# Network policies: every service a workload calls must admit that workload, and
# nothing that no one calls may be open to it. (default-deny means a missing
# allow is an outage that only shows up in a real cluster.)
allowed = {}   # target app name -> set of caller app names
for (kd, nm), d in by_kind.items():
    if kd != "NetworkPolicy" or nm in ("default-deny-ingress", "allow-ingress-controller"):
        continue
    target = d["spec"]["podSelector"]["matchLabels"]["app.kubernetes.io/name"]
    for rule in d["spec"]["ingress"]:
        for f in rule["from"]:
            allowed.setdefault(target, set()).add(f["podSelector"]["matchLabels"]["app.kubernetes.io/name"])
if ("NetworkPolicy", "default-deny-ingress") not in by_kind:
    problems.append("no default-deny NetworkPolicy: the allow rules would restrict nothing")
calls = {}
for (kd, nm), d in by_kind.items():
    if kd != "Deployment":
        continue
    me = d["spec"]["template"]["metadata"]["labels"]["app.kubernetes.io/name"]
    for c in d["spec"]["template"]["spec"]["containers"]:
        for e in c.get("env", []):
            v = e.get("value", "")
            if e["name"].endswith("_URL") and v.startswith("http://"):
                host = v[len("http://"):].split(":")[0]
                calls.setdefault(host, set()).add(me)
for target, callers in calls.items():
    if allowed.get(target, set()) != callers:
        problems.append(f"NetworkPolicy for {target} admits {sorted(allowed.get(target, []))}, but {sorted(callers)} call it")
ctl = by_kind.get(("NetworkPolicy", "allow-ingress-controller"))
if ctl:
    admitted = set(ctl["spec"]["podSelector"]["matchExpressions"][0]["values"])
    if admitted != {"web-service", "dex"}:
        problems.append(f"the ingress controller may reach {sorted(admitted)}; it should reach exactly web-service and dex")
    ns = ctl["spec"]["ingress"][0]["from"][0]["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"]
    if ns != "traefik":
        problems.append(f"the policy admits the ingress controller from namespace {ns!r}, but docs/operations.md installs Traefik into 'traefik'")

# The public address must read the same everywhere it appears, or sign-in breaks
# with an error that does not say why.
pub = {l.split("=", 1)[0]: l.split("=", 1)[1] for g in base_k["configMapGenerator"] if g["name"] == "gocart-public" for l in g["literals"]}
# Dex is configured by environment variables read by a template (dex/config.yaml),
# the same template compose uses. Every variable the template reads must be given
# by the Deployment (and by compose), and the address must be built from the
# shared public values so it cannot disagree with the gateway's.
import re
tpl = (ROOT/"base/dex/config.yaml").read_text()
wanted = set(re.findall(r'getenv "([A-Z0-9_]+)"', tpl))
dex_pod = by_kind[("Deployment", "dex")]["spec"]["template"]["spec"]
dex_env = {e["name"]: e for e in dex_pod["containers"][0]["env"]}
for name in sorted(wanted - set(dex_env)):
    problems.append(f"dex/config.yaml reads {name}, which the Dex Deployment does not set")
if "theme: gocart" not in tpl:
    problems.append("dex/config.yaml does not select the gocart theme")
def cmref(e):
    r = e.get("valueFrom", {}).get("configMapKeyRef")
    return (r["name"], r["key"]) if r else None
if cmref(dex_env.get("DEX_ISSUER", {})) != ("gocart-public", "oidc-issuer"):
    problems.append("Dex's issuer is not taken from gocart-public/oidc-issuer, so it could differ from the gateway's")
if cmref(dex_env.get("PUBLIC_URL", {})) != ("gocart-public", "public-url") or dex_env.get("GOCART_REDIRECT_URI", {}).get("value") != "$(PUBLIC_URL)/auth/callback":
    problems.append("Dex's redirect URI is not built from gocart-public/public-url + /auth/callback")
if not pub["oidc-issuer"].startswith(pub["public-url"] + "/"):
    problems.append(f"oidc-issuer {pub['oidc-issuer']} is not under public-url {pub['public-url']}")
mounts = {m["mountPath"]: m["name"] for m in dex_pod["containers"][0]["volumeMounts"]}
vols = {v["name"]: v for v in dex_pod["volumes"]}
for path, cm in (("/etc/dex", "dex-config"), ("/srv/dex/web/themes/gocart", "dex-theme")):
    v = vols.get(mounts.get(path), {})
    if v.get("configMap", {}).get("name") != cm:
        problems.append(f"Dex does not mount ConfigMap {cm} at {path}")
if "/tmp" not in mounts:
    problems.append("Dex has no writable /tmp: its image renders the config template there, and the root filesystem is read-only")
theme_files = {f.split("/")[-1] for g in base_k["configMapGenerator"] if g["name"] == "dex-theme" for f in g["files"]}
if not {"styles.css", "logo.png", "favicon.png"} <= theme_files:
    problems.append("the dex-theme ConfigMap lacks styles.css, logo.png or favicon.png")
for f in re.findall(r"url\(\"?([\w.-]+\.woff2)", (ROOT/"base/dex/theme/styles.css").read_text()):
    if f not in theme_files:
        problems.append(f"the Dex theme uses font {f}, which is not in the dex-theme ConfigMap")
# compose gives Dex the same variables
compose = yaml.safe_load((ROOT/"../../docker-compose.yml").read_text())
cenv = set(compose["services"]["dex"]["environment"])
for name in sorted(wanted - cenv):
    problems.append(f"dex/config.yaml reads {name}, which docker-compose.yml does not give Dex")
if ("./infra/k8s/base/dex/config.yaml:/etc/dex/config.yaml:ro" not in compose["services"]["dex"]["volumes"]):
    problems.append("docker-compose.yml does not mount the shared Dex config")
if pub["admin-emails"] and not pub["admin-emails"].strip():
    problems.append("admin-emails is blank")
host = pub["public-url"].split("://", 1)[1].split(":")[0]

# The edge layer is Traefik and cert-manager CRDs, which the Kubernetes schemas
# do not know, so check what matters by hand: routes land on real Services and
# ports, every middleware they name exists, the certificate covers the host and
# lands in the Secret the routes serve.
edge = {(d["kind"], d["metadata"]["name"]): d for d in docs(ROOT/"edge/edge.yaml")}
cert = edge.get(("Certificate", "gocart-tls"))
if not cert or host not in cert["spec"]["dnsNames"]:
    problems.append(f"edge: the certificate does not cover {host}")
elif cert["spec"]["issuerRef"]["name"] not in {d["metadata"]["name"] for d in docs(ROOT/"cert-manager/clusterissuers.yaml")}:
    problems.append(f"edge: the certificate asks for issuer {cert['spec']['issuerRef']['name']}, which cert-manager/clusterissuers.yaml does not define")
for (kd, nm), d in edge.items():
    if kd != "IngressRoute":
        continue
    if d["spec"].get("tls", {}).get("secretName") != (cert or {}).get("spec", {}).get("secretName"):
        problems.append(f"IngressRoute/{nm} serves a different TLS Secret than the certificate writes")
    if d["spec"]["entryPoints"] != ["websecure"]:
        problems.append(f"IngressRoute/{nm} is reachable without TLS")
    for r in d["spec"]["routes"]:
        if f"Host(`{host}`)" not in r["match"]:
            problems.append(f"IngressRoute/{nm}: rule {r['match']} is not for host {host}")
        for m in r.get("middlewares", []):
            if ("Middleware", m["name"]) not in edge:
                problems.append(f"IngressRoute/{nm}: uses Middleware {m['name']}, which is not defined")
        for sv in r["services"]:
            svc = by_kind.get(("Service", sv["name"]))
            if not svc:
                problems.append(f"IngressRoute/{nm}: sends traffic to Service {sv['name']}, which does not exist")
            elif sv["port"] not in {sp["name"] for sp in svc["spec"]["ports"]}:
                problems.append(f"IngressRoute/{nm}: Service {sv['name']} has no port named {sv['port']}")
        if "/auth" in r["match"] and not any(m["name"] == "gocart-limit-signin" for m in r.get("middlewares", [])):
            problems.append(f"IngressRoute/{nm}: /auth is not behind the strict sign-in rate limit")
if pub["public-url"].split("://")[0] != "https":
    problems.append("public-url is not https: session cookies would not be Secure")

# the REQUIRE_IDENTITY services must have a secret to verify with
for nm in ("order-service", "cart-service"):
    envs = {e["name"] for e in by_kind[("Deployment", nm)]["spec"]["template"]["spec"]["containers"][0]["env"]}
    if not {"IDENTITY_SECRET", "REQUIRE_IDENTITY"} <= envs:
        problems.append(f"{nm} does not require a signed identity")

# 3. Kustomizations: every referenced path exists, images name real images, and
#    the JSON-patch path used by the cassandra overlay resolves in the base.
def kust(p): return yaml.safe_load((ROOT/p/"kustomization.yaml").read_text())
for p in ["base", "cassandra", "edge", "overlays/local", "overlays/local-https", "overlays/staging", "overlays/staging-cassandra"]:
    k = kust(p)
    for r in k.get("resources", []) + k.get("components", []):
        t = (ROOT/p/r).resolve()
        if not t.exists():
            problems.append(f"{p}: resource {r} does not exist")
        elif t.is_dir() and not (t/"kustomization.yaml").exists():
            problems.append(f"{p}: {r} is a directory without a kustomization.yaml")

base_images = {c["image"].rsplit(":", 1)[0]
               for (kd, _), d in by_kind.items() if kd == "Deployment"
               for c in d["spec"]["template"]["spec"]["containers"]}
for im in kust("overlays/staging")["images"]:
    if im["name"] not in base_images:
        problems.append(f"overlays/staging renames {im['name']}, which no Deployment uses")
renamed = {im["newName"] for im in kust("overlays/staging")["images"]}
for im in kust("overlays/staging-cassandra")["images"]:
    if im["name"] not in renamed:
        problems.append(f"overlays/staging-cassandra retags {im['name']}, which staging does not produce")

patch = yaml.safe_load(kust("overlays/staging-cassandra")["patches"][0]["patch"])
node = order
if not isinstance(order.get("env"), list):
    problems.append("order-service container has no env list for the JSON patch to append to")
for op in patch:
    if not op["path"].endswith("/env/-") or "/containers/0/" not in op["path"]:
        problems.append(f"unexpected patch path {op['path']}")

# The scale-out patch must leave nothing mounted that no longer exists, and
# must only touch paths the base really has.
scale = yaml.safe_load(kust("overlays/staging-cassandra")["patches"][1]["patch"])
spec = by_kind[("Deployment", "order-service")]["spec"]
for op in scale:
    parts = [x for x in op["path"].split("/") if x]
    node = spec if parts[0] == "spec" else None
    cur = {"spec": spec}
    try:
        for x in parts:
            cur = cur[int(x)] if isinstance(cur, list) else cur[x]
    except (KeyError, IndexError, ValueError):
        problems.append(f"scale-out patch path {op['path']} does not exist in the base")
ops = {op["path"]: op for op in scale}
if ops.get("/spec/replicas", {}).get("value", 1) < 2:
    problems.append("staging-cassandra does not scale order-service past one replica")
if "/spec/template/spec/volumes" not in ops or "/spec/template/spec/containers/0/volumeMounts" not in ops:
    problems.append("staging-cassandra scales out but still mounts the file store's volume")

# the tag in staging must equal VERSION
version = (ROOT/"../../VERSION").read_text().strip()
for im in kust("overlays/staging")["images"]:
    if im["newTag"] != version:
        problems.append(f"overlays/staging tag {im['newTag']} != VERSION {version}")
for im in kust("overlays/staging-cassandra")["images"]:
    if im["newTag"] != version + "-cassandra":
        problems.append(f"overlays/staging-cassandra tag {im['newTag']} != {version}-cassandra")
for (kd, nm), d in by_kind.items():
    if kd == "Deployment" and d["spec"]["template"]["spec"]["containers"][0]["image"].startswith("gocart/"):
        tag = d["spec"]["template"]["spec"]["containers"][0]["image"].rsplit(":", 1)[1]
        if tag != version:
            problems.append(f"base {nm} image tag {tag} != VERSION {version}")

print("PROBLEMS:" if problems else "structure checks: all passed")
for p in problems: print("  -", p)
sys.exit(1 if problems else 0)
