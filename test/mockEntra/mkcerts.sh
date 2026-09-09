#!/usr/bin/env sh
# Issues the certificate the mock answers with, and the authority ZITADEL is
# told to trust.
#
# ZITADEL's Microsoft connector talks to `login.microsoftonline.com` and
# `graph.microsoft.com` over TLS and will not be talked out of it, so a mock
# that answers to those names has to present a certificate for them. The
# authority here is generated locally, lives in a directory that is not
# tracked, and is trusted by exactly two containers in one compose overlay.
#
# It is NOT in the repository. A private key that ships with a project is a
# private key that ends up trusted somewhere it was never meant to be, and a
# secret scanner is right to object to it. Run this instead: it takes a second
# and needs only openssl.
set -eu

out="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/certs"
mkdir -p "$out"

if [ -f "$out/mock.crt" ] && [ -f "$out/ca.crt" ] && [ "${FORCE:-}" = "" ]; then
  echo "certificates already present in $out (FORCE=1 to reissue)"
  exit 0
fi

cat > "$out/openssl.cnf" <<'CNF'
[req]
distinguished_name = dn
[dn]
[leaf]
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:login.microsoftonline.com, DNS:graph.microsoft.com, DNS:mock-entra, DNS:localhost, IP:127.0.0.1
CNF

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -subj "/CN=Mock Entra local test authority" \
  -keyout "$out/ca.key" -out "$out/ca.crt" 2>/dev/null

openssl req -newkey rsa:2048 -nodes \
  -subj "/CN=login.microsoftonline.com" \
  -keyout "$out/mock.key" -out "$out/mock.csr" 2>/dev/null

openssl x509 -req -in "$out/mock.csr" -days 3650 \
  -CA "$out/ca.crt" -CAkey "$out/ca.key" -CAcreateserial \
  -extfile "$out/openssl.cnf" -extensions leaf \
  -out "$out/mock.crt" 2>/dev/null

rm -f "$out/mock.csr" "$out/openssl.cnf" "$out/ca.srl"
chmod 644 "$out/mock.key" "$out/mock.crt" "$out/ca.crt"
echo "issued $out/mock.crt for login.microsoftonline.com and graph.microsoft.com"
