#!/bin/sh
# Fills in nginx.conf's __DNS_RESOLVER__ with this engine's actual embedded
# DNS address before nginx starts. Docker's is always 127.0.0.11; Podman's
# (aardvark-dns) is the network's own gateway, a different IP per network, so
# it is read from /etc/resolv.conf rather than assumed.
set -eu

RESOLVER=$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)
RESOLVER=${RESOLVER:-127.0.0.11}
sed -i "s/__DNS_RESOLVER__/$RESOLVER/" /etc/nginx/conf.d/app.conf

exec nginx -g 'daemon off;'
