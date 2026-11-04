#!/usr/bin/env sh
# Prints a .env with freshly generated secrets, from .env.example (local
# development) or the template given (deploy/staging/env.example).
#
#   scripts/gen-env.sh [template] > .env
set -eu
template="${1:-$(dirname "$0")/../.env.example}"
rnd() { openssl rand -hex "$1"; }
PG=$(rnd 24); JWT=$(rnd 32); REDIS=$(rnd 24); LK_KEY="API$(rnd 6)"; LK_SECRET=$(rnd 32)
S3_KEY=$(rnd 12); S3_SECRET=$(rnd 24); CDN=$(rnd 32)
sed \
  -e "s|^RUSCORD_PG_PASSWORD=.*|RUSCORD_PG_PASSWORD=$PG|" \
  -e "s|^RUSCORD_JWT_ED25519_SEED=.*|RUSCORD_JWT_ED25519_SEED=$JWT|" \
  -e "s|^RUSCORD_REDIS_PASSWORD=.*|RUSCORD_REDIS_PASSWORD=$REDIS|" \
  -e "s|<RUSCORD_PG_PASSWORD>|$PG|" \
  -e "s|^RUSCORD_LIVEKIT_API_KEY=.*|RUSCORD_LIVEKIT_API_KEY=$LK_KEY|" \
  -e "s|^RUSCORD_LIVEKIT_API_SECRET=.*|RUSCORD_LIVEKIT_API_SECRET=$LK_SECRET|" \
  -e "s|^RUSCORD_S3_ACCESS_KEY=.*|RUSCORD_S3_ACCESS_KEY=$S3_KEY|" \
  -e "s|^RUSCORD_S3_SECRET_KEY=.*|RUSCORD_S3_SECRET_KEY=$S3_SECRET|" \
  -e "s|^RUSCORD_CDN_SECRET=.*|RUSCORD_CDN_SECRET=$CDN|" \
  "$template"
