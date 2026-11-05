#!/usr/bin/env bash
# Change the release version everywhere it appears, in one step.
#
#   ./scripts/bump-version.sh 0.2.0
#
# The version lives in the VERSION file and is repeated in the Kubernetes
# manifests and compose files. Left to hand edits those drift apart, and then
# staging runs a different image than the one you just shipped. This updates all
# of them and re-checks that they agree.
set -euo pipefail
cd "$(dirname "$0")/.."

new=${1:-}
if [[ ! $new =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 MAJOR.MINOR.PATCH   (for example 0.2.0)" >&2
  exit 2
fi
old=$(<VERSION)
old=${old//$'\n'/}
if [[ $old == "$new" ]]; then
  echo "already at $new"
  exit 0
fi
esc=${old//./\\.}

printf '%s\n' "$new" > VERSION
sed -i -E "s#(image: gocart/(product|order|payment)-service:)${esc}\$#\1${new}#" infra/k8s/base/*.yaml
sed -i -E "s#(newTag: )${esc}\$#\1${new}#" infra/k8s/overlays/staging/kustomization.yaml
sed -i -E "s#(newTag: )${esc}-cassandra\$#\1${new}-cassandra#" infra/k8s/overlays/staging-cassandra/kustomization.yaml
sed -i -E "s#\\\$\{VERSION:-${esc}\}#\${VERSION:-${new}}#g" docker-compose.yml docker-compose.cassandra.yml

echo "$old -> $new"
if python3 -c 'import kubernetes_validate, yaml' 2>/dev/null; then
  python3 scripts/check-k8s.py
else
  echo "(install pyyaml and kubernetes-validate to have scripts/check-k8s.py verify the result)"
fi
grep -rn --include='*.yml' --include='*.yaml' -F "$old" docker-compose*.yml infra/k8s || true
