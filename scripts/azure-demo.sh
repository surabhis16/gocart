#!/usr/bin/env bash
# One command to put GoCart on Azure over HTTPS, and one to take it away.
#
#   ./scripts/azure-demo.sh up <name>     create a VM that serves https://<name>.<region>.cloudapp.azure.com
#   ./scripts/azure-demo.sh status        is it up yet?
#   ./scripts/azure-demo.sh down          delete everything (stops all charges)
#
# Run it where `az` is signed in, for example Azure Cloud Shell:
#   git clone https://github.com/surabhis16/gocart.git && cd gocart && ./scripts/azure-demo.sh up mygocart
#
# What `up` does: creates a resource group and an Ubuntu VM with a free Azure DNS name,
# opens ports 80 and 443 (and 22 for SSH) only, and gives the VM a start-up script that
# installs Docker, fetches this repository at the release tag, writes a .env with fresh
# random secrets, pulls the published release images from ghcr.io and starts the stack
# behind Caddy, which gets a Let's Encrypt certificate. Allow 5 to 10 minutes.
#
# Settings (environment variables, all optional):
#   RG=gocart-demo  LOCATION=indiasouthcentral  SIZE=Standard_B2s
#   REF=<git tag or branch>         default: v<VERSION>, the release matching this checkout
#                                   (it must contain docker-compose.https.yml; until a release does, use REF=main)
#   BUILD=1                         build the images on the VM instead of pulling release images
#   DEX_DEMO_USERS=true|false       the demo accounts (default true; they are public, so turn
#                                   them off for anything you leave running)
#   ADMIN_EMAILS=you@example.com    who is the administrator (default admin@gocart.test)
#   GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET / GITHUB_CLIENT_ID / GITHUB_CLIENT_SECRET
#                                   turn on Google or GitHub sign-in; register the callback
#                                   https://<host>/dex/callback, which `up` prints
#   DRY_RUN=1                       print the start-up script and the az commands, change nothing
set -euo pipefail

RG=${RG:-gocart-demo}
LOCATION=${LOCATION:-indiasouthcentral}
SIZE=${SIZE:-Standard_B2s}
REPO=${REPO:-https://github.com/surabhis16/gocart.git}
VM=gocart-vm
ADMIN_USER=azureuser
here=$(cd "$(dirname "$0")/.." && pwd)
init=""   # the start-up script is written to a temp file (it holds secrets) and removed on exit
trap 'rm -f "${init:-}"' EXIT

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

run() { if [ -n "${DRY_RUN:-}" ]; then printf '+ %s\n' "$*"; else "$@"; fi; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "$1 is required" >&2; exit 1; }; }

rand() { openssl rand -hex 24; }

up() {
  local label=${1:-}
  [[ $label =~ ^[a-z][a-z0-9-]{1,61}[a-z0-9]$ ]] || {
    echo "name must be 3-63 characters: lowercase letters, digits and hyphens, starting with a letter" >&2; exit 2; }
  [ -n "${DRY_RUN:-}" ] || need az
  need openssl

  local version ref host
  version=$(tr -d '[:space:]' < "$here/VERSION" 2>/dev/null || echo "")
  ref=${REF:-${version:+v$version}}
  ref=${ref:-main}
  host="$label.$LOCATION.cloudapp.azure.com"

  # The ref must already contain the HTTPS files, or the VM would start without Caddy.
  local slug code
  slug=$(sed -E 's#^https://github\.com/##; s#\.git$##' <<<"$REPO")
  code=$(curl -s -o /dev/null -m 15 -w '%{http_code}' "https://raw.githubusercontent.com/$slug/$ref/docker-compose.https.yml" || true)
  case $code in
    200) ;;
    404) echo "$ref of $REPO does not contain docker-compose.https.yml yet." >&2
         echo "Push it (and tag a new release), or deploy a branch that has it: REF=main $0 up $label" >&2
         exit 1 ;;
    *)   echo "(could not check that $ref contains docker-compose.https.yml; continuing)" >&2 ;;
  esac

  local compose_file="docker-compose.yml:docker-compose.https.yml"
  [ -n "${BUILD:-}" ] || compose_file="$compose_file:docker-compose.ghcr.yml"

  init=$(mktemp)
  {
    echo "#cloud-config"
    echo "write_files:"
    echo "  - path: /root/gocart.env"
    echo "    permissions: '0600'"
    echo "    content: |"
    {
      echo "COMPOSE_FILE=$compose_file"
      echo "PUBLIC_HOST=$host"
      echo "SESSION_SECRET=$(rand)"
      echo "IDENTITY_SECRET=$(rand)"
      echo "ADMIN_KEY=$(rand)"
      echo "OIDC_CLIENT_SECRET=$(rand)"
      echo "DEX_DEMO_USERS=${DEX_DEMO_USERS:-true}"
      echo "ADMIN_EMAILS=${ADMIN_EMAILS:-admin@gocart.test}"
      echo "GOOGLE_CLIENT_ID=${GOOGLE_CLIENT_ID:-}"
      echo "GOOGLE_CLIENT_SECRET=${GOOGLE_CLIENT_SECRET:-}"
      echo "GITHUB_CLIENT_ID=${GITHUB_CLIENT_ID:-}"
      echo "GITHUB_CLIENT_SECRET=${GITHUB_CLIENT_SECRET:-}"
    } | sed 's/^/      /'
    echo "runcmd:"
    echo "  - curl -fsSL https://get.docker.com | sh"
    echo "  - usermod -aG docker $ADMIN_USER"
    echo "  - git clone --branch $ref $REPO /opt/gocart"
    echo "  - cp /root/gocart.env /opt/gocart/.env && chmod 600 /opt/gocart/.env"
    echo "  - chown -R $ADMIN_USER:$ADMIN_USER /opt/gocart"
    if [ -n "${BUILD:-}" ]; then
      echo "  - cd /opt/gocart && docker compose up -d --build"
    else
      echo "  - cd /opt/gocart && docker compose pull && docker compose up -d --no-build"
    fi
  } > "$init"

  if [ -n "${DRY_RUN:-}" ]; then
    echo "--- start-up script (secrets hidden) ---"
    sed -E 's/((SESSION|IDENTITY|OIDC_CLIENT)_SECRET|ADMIN_KEY|GOOGLE_CLIENT_SECRET|GITHUB_CLIENT_SECRET)=.+/\1=<hidden>/' "$init"
    echo "---"
  fi

  echo "Creating $VM in $RG ($LOCATION), serving https://$host"
  run az group create -n "$RG" -l "$LOCATION" -o none
  run az vm create -g "$RG" -n "$VM" --image Ubuntu2204 --size "$SIZE" \
    --admin-username "$ADMIN_USER" --generate-ssh-keys \
    --public-ip-sku Standard --public-ip-address-dns-name "$label" \
    --custom-data "$init" -o none
  run az vm open-port -g "$RG" -n "$VM" --port 80 --priority 1010 -o none
  run az vm open-port -g "$RG" -n "$VM" --port 443 --priority 1020 -o none

  cat <<MSG

The VM is created and is now installing GoCart (5 to 10 minutes).
  Shop:      https://$host
  Check:     ./scripts/azure-demo.sh status
  Logs:      ssh $ADMIN_USER@$host 'cd /opt/gocart && docker compose logs -f --tail 50'
  Callback to register for Google and GitHub sign-in:  https://$host/dex/callback
  When done: ./scripts/azure-demo.sh down
MSG
  if [ "${DEX_DEMO_USERS:-true}" = true ]; then
    echo "  The demo accounts are on, and their password is public: delete the VM when you are done."
  fi
}

status() {
  need az
  local fqdn
  fqdn=$(az network public-ip show -g "$RG" -n "${VM}PublicIP" --query dnsSettings.fqdn -o tsv 2>/dev/null || true)
  [ -n "$fqdn" ] || { echo "no VM found in resource group $RG"; exit 1; }
  echo "VM: $(az vm get-instance-view -g "$RG" -n "$VM" --query "instanceView.statuses[1].displayStatus" -o tsv)"
  local code
  code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "https://$fqdn/healthz" || true)
  if [ "$code" = 200 ]; then echo "https://$fqdn is up"; else echo "https://$fqdn is not answering yet (got '${code:-none}'); installing takes 5 to 10 minutes"; fi
}

down() {
  need az
  if [ "${2:-}" != "-y" ]; then
    read -r -p "Delete resource group $RG and everything in it? [y/N] " a
    [ "$a" = y ] || { echo "kept"; exit 0; }
  fi
  az group delete -n "$RG" --yes --no-wait
  echo "deleting $RG (finishes in a few minutes)"
}

case ${1:-} in
  up) shift; up "$@" ;;
  status) status ;;
  down) down "$@" ;;
  *) usage ;;
esac
