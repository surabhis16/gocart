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
             ROOT/"base/payment-service.yaml", ROOT/"base/cart-service.yaml",
             ROOT/"cassandra/cassandra.yaml", ROOT/"overlays/local/namespace.yaml",
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

# Every secret a workload reads must be produced by every overlay that deploys it.
needed = set()
for (kd, _), d in by_kind.items():
    if kd == "Deployment":
        for c in d["spec"]["template"]["spec"]["containers"]:
            for e in c.get("env", []):
                ref = e.get("valueFrom", {}).get("secretKeyRef")
                if ref:
                    needed.add((ref["name"], ref["key"]))
for ov in ("overlays/local", "overlays/staging"):
    gen = {}
    for g in yaml.safe_load((ROOT/ov/"kustomization.yaml").read_text()).get("secretGenerator", []):
        for lit in g.get("literals", []):
            gen[(g["name"], lit.split("=", 1)[0])] = True
    for name, key in needed:
        if (name, key) not in gen:
            problems.append(f"{ov} does not generate secret {name}/{key}, which a Deployment reads")

# 3. Kustomizations: every referenced path exists, images name real images, and
#    the JSON-patch path used by the cassandra overlay resolves in the base.
def kust(p): return yaml.safe_load((ROOT/p/"kustomization.yaml").read_text())
for p in ["base", "cassandra", "overlays/local", "overlays/staging", "overlays/staging-cassandra"]:
    k = kust(p)
    for r in k.get("resources", []):
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
    if kd == "Deployment":
        tag = d["spec"]["template"]["spec"]["containers"][0]["image"].rsplit(":", 1)[1]
        if tag != version:
            problems.append(f"base {nm} image tag {tag} != VERSION {version}")

print("PROBLEMS:" if problems else "structure checks: all passed")
for p in problems: print("  -", p)
sys.exit(1 if problems else 0)
