#!/usr/bin/env sh
# Prints a .env for local development with freshly generated secrets.
set -eu
rnd() { openssl rand -hex "$1"; }
PG=$(rnd 24); REDIS=$(rnd 24); LK_KEY="API$(rnd 6)"; LK_SECRET=$(rnd 32)
sed \
  -e "s|^RUSCORD_PG_PASSWORD=.*|RUSCORD_PG_PASSWORD=$PG|" \
  -e "s|^RUSCORD_REDIS_PASSWORD=.*|RUSCORD_REDIS_PASSWORD=$REDIS|" \
  -e "s|<RUSCORD_PG_PASSWORD>|$PG|" \
  -e "s|^RUSCORD_LIVEKIT_API_KEY=.*|RUSCORD_LIVEKIT_API_KEY=$LK_KEY|" \
  -e "s|^RUSCORD_LIVEKIT_API_SECRET=.*|RUSCORD_LIVEKIT_API_SECRET=$LK_SECRET|" \
  "$(dirname "$0")/../.env.example"
