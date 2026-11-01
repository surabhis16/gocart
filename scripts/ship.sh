#!/usr/bin/env bash
# Build GoCart's images, ship them to a registry, and deploy them to a local
# Kubernetes "staging" namespace on minikube.
#
#   ./scripts/ship.sh              # events in a file (default)
#   ./scripts/ship.sh cassandra    # events in Cassandra: slow first start, ~1.5 GB RAM
#
# Needs: docker, kubectl, minikube (running).  Free: all of it is local.
# Undo it with ./scripts/cleanup.sh.
#
# Using Docker Desktop's Kubernetes instead of minikube? It shares your Docker
# image store, so skip the registry and run:
#   docker compose build && kubectl apply -k infra/k8s/overlays/local
#
# How the registry works: minikube's `registry` addon runs a registry inside the
# cluster and makes it reachable at localhost:5000 from every node. A
# port-forward makes the same registry reachable at localhost:5000 from this
# machine. So `docker push localhost:5000/...` here, and the cluster pulls the
# very same name. Nothing leaves your computer.
set -euo pipefail
cd "$(dirname "$0")/.."

variant=${1:-file}
case $variant in
  file)      overlay=staging;           tag_suffix="" ;;
  cassandra) overlay=staging-cassandra; tag_suffix="-cassandra" ;;
  *) echo "usage: $0 [file|cassandra]" >&2; exit 2 ;;
esac

version=$(<VERSION)
version=${version//$'\n'/}
registry=localhost:5000
ns=gocart-staging
pf_pid=

need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 1; }; }
need docker; need kubectl; need minikube; need curl

cleanup() { [[ -n $pf_pid ]] && kill "$pf_pid" 2>/dev/null || true; }
trap cleanup EXIT

echo "==> Checking that the manifests agree on version $version"
if python3 -c 'import kubernetes_validate, yaml' 2>/dev/null; then
  python3 scripts/check-k8s.py
else
  grep -q "newTag: ${version}${tag_suffix}\$" "infra/k8s/overlays/${overlay}/kustomization.yaml" || {
    echo "overlay ${overlay} does not use tag ${version}${tag_suffix}; run scripts/bump-version.sh ${version}" >&2
    exit 1
  }
fi

echo "==> Enabling minikube's registry addon"
minikube addons enable registry >/dev/null
kubectl -n kube-system rollout status deployment/registry --timeout=180s

echo "==> Opening $registry on this machine"
kubectl -n kube-system port-forward service/registry 5000:80 >/dev/null 2>&1 &
pf_pid=$!
for _ in $(seq 1 30); do
  curl -fsS "http://${registry}/v2/" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://${registry}/v2/" >/dev/null || { echo "registry is not reachable on $registry" >&2; exit 1; }

ship() { # service tag [docker build args...]
  local svc=$1 tag=$2; shift 2
  local ref="${registry}/gocart/${svc}:${tag}"
  echo "==> Building and pushing ${ref}"
  docker build --build-arg "VERSION=${version}" "$@" -t "$ref" "./${svc}"
  docker push "$ref"
}
ship product-service "$version"
if [[ $variant == cassandra ]]; then
  ship order-service "${version}${tag_suffix}" --build-arg TAGS=cassandra
else
  ship order-service "$version"
fi

echo "==> Deploying overlay ${overlay} to namespace ${ns}"
kubectl apply -k "infra/k8s/overlays/${overlay}"
if [[ $variant == cassandra ]]; then
  echo "    Cassandra needs a minute or more to start; the Order service waits for it."
  kubectl -n "$ns" rollout status statefulset/cassandra --timeout=600s
fi
kubectl -n "$ns" rollout status deployment/product-service --timeout=180s
kubectl -n "$ns" rollout status deployment/order-service --timeout=600s

cat <<MSG

Staging is up. Reach it from this machine with:

  kubectl -n ${ns} port-forward svc/product-service 8080:8080 &
  kubectl -n ${ns} port-forward svc/order-service   8081:8081 &
  ./scripts/smoke.sh

Show the outage-and-recovery behaviour here:

  STOP_PRODUCT='kubectl -n ${ns} scale deploy/product-service --replicas=0' \\
  START_PRODUCT='kubectl -n ${ns} scale deploy/product-service --replicas=1' \\
  ./scripts/outage-demo.sh

Tear it all down: ./scripts/cleanup.sh
MSG
