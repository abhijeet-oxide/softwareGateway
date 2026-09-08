#!/bin/sh
# Renders deploy/zitadel/nginx.conf into place with this engine's embedded DNS
# address, then starts nginx.
#
# The config is bind-mounted READ ONLY, so it is rendered THROUGH a pipe into
# nginx's own directory rather than edited in place the way the web tier's
# entrypoint does it - an in-place sed against a read-only mount fails, and it
# fails at container start where nobody is looking.
#
# The resolver address itself cannot be hardcoded: Docker's is always
# 127.0.0.11, Podman's (aardvark-dns) is the network's own gateway and differs
# per network. See deploy/web/nginx.conf for why it is needed at all.
set -eu

RESOLVER=$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)
RESOLVER=${RESOLVER:-127.0.0.11}

# The stock image ships a default server on port 80. Left in place it is a
# second default_server and nginx refuses to start.
rm -f /etc/nginx/conf.d/default.conf

sed "s/__DNS_RESOLVER__/$RESOLVER/" /etc/nginx/templates/zitadel.conf \
  > /etc/nginx/conf.d/app.conf

exec nginx -g 'daemon off;'
