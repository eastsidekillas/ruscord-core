#!/usr/bin/env sh
# Checks the test server from the outside, through its domain and HTTPS:
# the client, the API, the core's readiness on the server itself, and
# LiveKit: the local one's signalling behind /livekit and its media ports,
# or for LiveKit Cloud the webhook path to the core and the project.
#
#   scripts/staging-check.sh            domain from .env (RUSCORD_DOMAIN)
#   scripts/staging-check.sh example.ru
#   RUSCORD_CHECK_CACERT=root.crt     trust this CA too (CI: Caddy's local CA)
set -eu
cd "$(dirname "$0")/.."
domain="${1:-$(sed -n 's/^RUSCORD_DOMAIN=//p' .env 2>/dev/null)}"
[ -n "$domain" ] || { echo "no domain: pass it or set RUSCORD_DOMAIN in .env"; exit 2; }
fail=0
check() { # name, expected HTTP status, url
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 ${RUSCORD_CHECK_CACERT:+--cacert "$RUSCORD_CHECK_CACERT"} "$3" || true)
  if [ "$code" = "$2" ]; then echo "ok   $1 ($code)"; else echo "FAIL $1: $3 answered $code, want $2"; fail=1; fi
}
check "client over HTTPS" 200 "https://$domain/"
check "API answers, sign-in required" 401 "https://$domain/api/v1/users/@me"
check "core ready (on the server)" 200 "http://127.0.0.1:8080/readyz"
mode="$(sed -n 's/^RUSCORD_LIVEKIT_MODE=//p' .env 2>/dev/null)"
if [ "$mode" = cloud ]; then
  # LiveKit Cloud: its webhooks reach the core (an unsigned one is refused),
  # and the project answers.
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 ${RUSCORD_CHECK_CACERT:+--cacert "$RUSCORD_CHECK_CACERT"} -X POST "https://$domain/livekit/webhook" || true)
  if [ "$code" = 401 ]; then echo "ok   LiveKit Cloud webhooks reach the core (401 unsigned)"; else echo "FAIL webhook path answered $code, want 401 from the core"; fail=1; fi
  cloud="$(sed -n 's/^RUSCORD_LIVEKIT_CLOUD_URL=//p' .env | sed 's#^wss://#https://#')"
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$cloud" || true)
  if [ -n "$cloud" ] && [ "$code" != 000 ]; then echo "ok   LiveKit Cloud project answers ($code)"; else echo "FAIL LiveKit Cloud project $cloud unreachable"; fail=1; fi
else
  check "LiveKit signalling" 200 "https://$domain/livekit/"
  for p in 7881/tcp 7882/udp; do
    if docker ps --format '{{.Ports}}' | grep -q "0.0.0.0:${p%/*}->${p}"; then echo "ok   LiveKit $p published"; else echo "FAIL LiveKit $p not published"; fail=1; fi
  done
fi
exit $fail
