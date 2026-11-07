#!/usr/bin/env bash
# Remove everything ship.sh (or the local overlay) created.
#
#   ./scripts/cleanup.sh
#
# Deleting a namespace deletes everything in it: Deployments, Services, the
# volumes holding order, payment, cart and catalogue data, and Cassandra if it was deployed.
# That is the point; "clean up" should leave nothing running and nothing billed.
set -euo pipefail
cd "$(dirname "$0")/.."

if command -v kubectl >/dev/null && kubectl version --request-timeout=5s >/dev/null 2>&1; then
  echo "==> Deleting namespaces (and everything in them)"
  kubectl delete namespace gocart-staging gocart-local --ignore-not-found --wait=true
else
  echo "==> No reachable cluster; skipping Kubernetes clean-up"
fi

echo "==> Stopping any leftover registry port-forward"
pkill -f 'port-forward service/registry 5000:80' 2>/dev/null || true

if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
  version=$(<VERSION)
  version=${version//$'\n'/}
  echo "==> Removing local images"
  for ref in \
    "gocart/product-service:${version}" "gocart/payment-service:${version}" "gocart/cart-service:${version}" "gocart/web-service:${version}" "gocart/order-service:${version}" "gocart/order-service:${version}-cassandra" \
    "localhost:5000/gocart/product-service:${version}" "localhost:5000/gocart/payment-service:${version}" "localhost:5000/gocart/cart-service:${version}" "localhost:5000/gocart/web-service:${version}" "localhost:5000/gocart/order-service:${version}" \
    "localhost:5000/gocart/order-service:${version}-cassandra"; do
    docker image rm "$ref" >/dev/null 2>&1 && echo "    removed $ref" || true
  done
  echo "==> Stopping the compose stack and deleting its volumes"
  docker compose down -v --remove-orphans 2>/dev/null || true
fi
echo "Clean."
