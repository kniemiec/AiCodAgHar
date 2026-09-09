#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p certs
cd certs

if [ -f ca.key ]; then
  echo "certs already exist in deploy/certs/, skipping (delete the directory to regenerate)"
  exit 0
fi

# Dev CA.
openssl req -x509 -newkey rsa:2048 -days 3650 -nodes \
  -keyout ca.key -out ca.crt -subj "/CN=orchestrator-mvp-dev-ca" 2>/dev/null

# Server cert for launcher-mock. SANs cover both the in-compose-network
# hostname and host-based access (e.g. local integration test runs).
openssl req -newkey rsa:2048 -nodes -keyout server.key -out server.csr \
  -subj "/CN=launcher-mock" 2>/dev/null
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 3650 \
  -extfile <(printf "subjectAltName=DNS:launcher-mock,DNS:localhost,IP:127.0.0.1") 2>/dev/null

# Client cert for control-plane (and any other authorized caller, e.g. local
# integration test runs against the launcher-mock compose service).
openssl req -newkey rsa:2048 -nodes -keyout client.key -out client.csr \
  -subj "/CN=control-plane" 2>/dev/null
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out client.crt -days 3650 \
  -extfile <(printf "subjectAltName=DNS:control-plane") 2>/dev/null

rm -f server.csr client.csr ca.srl
echo "generated dev mTLS certs in deploy/certs/"
