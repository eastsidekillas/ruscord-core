#!/usr/bin/env sh
# Checks the test server from the outside, through its domain and HTTPS:
# the client, API, Core readiness and local LiveKit signalling/media when the
# self-hosted LiveKit mode is selected.
#
#   scripts/staging-check.sh            domain from .env (RUSCORD_DOMAIN)
#   scripts/staging-check.sh example.ru
#   RUSCORD_CHECK_CACERT=root.crt     trust this CA too (CI: Caddy's local CA)
set -eu
cd "$(dirname "$0")/.."
domain="${1:-$(sed -n 's/^RUSCORD_DOMAIN=//p' .env 2>/dev/null)}"
mode="$(sed -n 's/^RUSCORD_LIVEKIT_MODE=//p' .env 2>/dev/null | tail -1)"
mode="${mode:-local}"
[ -n "$domain" ] || { echo "no domain: pass it or set RUSCORD_DOMAIN in .env"; exit 2; }
fail=0
check() { # name, expected HTTP status, url
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 ${RUSCORD_CHECK_CACERT:+--cacert "$RUSCORD_CHECK_CACERT"} "$3" || true)
  if [ "$code" = "$2" ]; then echo "ok   $1 ($code)"; else echo "FAIL $1: $3 answered $code, want $2"; fail=1; fi
}
check_webhook_route() {
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST \
    -H 'Content-Type: application/webhook+json' --data '{}' \
    ${RUSCORD_CHECK_CACERT:+--cacert "$RUSCORD_CHECK_CACERT"} \
    "https://$domain/livekit/webhook" || true)
  if [ "$code" = 401 ]; then echo "ok   webhook route reaches Core (unsigned request rejected) ($code)"; else
    echo "FAIL webhook route: https://$domain/livekit/webhook answered $code, want 401"; fail=1
  fi
}
check "client over HTTPS" 200 "https://$domain/"
check "API answers, sign-in required" 401 "https://$domain/api/v1/users/@me"
check_webhook_route
check "core ready (on the server)" 200 "http://127.0.0.1:8080/readyz"
if [ "$mode" = local ]; then
  check "LiveKit signalling" 200 "https://$domain/livekit/"
  for p in 7881/tcp 7882/udp; do
    if docker ps --format '{{.Ports}}' | grep -q "0.0.0.0:${p%/*}->${p}"; then echo "ok   LiveKit $p published"; else echo "FAIL LiveKit $p not published"; fail=1; fi
  done
elif [ "$mode" = cloud ]; then
  echo "skip local LiveKit signalling and media ports (Cloud mode)"
else
  echo "FAIL RUSCORD_LIVEKIT_MODE must be local or cloud (got $mode)"
  fail=1
fi
exit $fail
