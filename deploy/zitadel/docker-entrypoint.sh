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

# THE REFUSAL, in one sentence.
#
# A person the directory authenticated but this system does not know is refused
# INSIDE ZITADEL: they read its Account Not Found page and never reach the
# application, so nothing the product draws can speak to them. What that page
# says by default is written for whoever built the integration:
#
#   We couldn't find an account associated with your identity provider
#   credentials.
#   No existing account was found. Please sign in with an existing account or
#   contact your administrator for assistance.
#
# Two sentences, one of them about identity providers and credentials, and
# neither naming anybody to ask. The reader is a colleague who has been told no.
# What they need is the fact and the address:
#
#   This account is not registered. Kindly reach out to <address> for access.
#
# The address is SUPPORT_CONTACT, which defaults to the bootstrap administrator
# - the person who provisions accounts here. With neither set the sentence still
# completes and names a role instead, because an invented address sends people
# to a mailbox nobody reads.
#
# DONE IN CSS, and that is not a shortcut. The obvious version - rewrite the
# sentence in the HTML with sub_filter - was written, deployed and watched: the
# server sends the new text, React hydrates over it, and the original is back
# before anybody reads it. A stylesheet is not hydrated. The cost is that the
# address is text rather than a mailto link.
#
# The anchors are the paragraphs' i18n keys, not their English text, so a
# deployment running in another language is matched too. If a later login image
# renames them the rules stop matching and the page reads as it always did,
# which is cosmetic and not a broken sign-in.
inc=/etc/nginx/conf.d/support-contact.inc
: > "$inc"
if [ -n "${SUPPORT_CONTACT:-}" ]; then
  line="This account is not registered. Kindly reach out to ${SUPPORT_CONTACT} for access."
else
  line="This account is not registered. Kindly reach out to an administrator for access."
fi
info='[data-i18n-key="idp.accountNotFound.info"]'
desc='[data-i18n-key="idp.accountNotFound.description"]'
# </title> rather than </head>: the branding rule in the login location already
# claims </head>, and two sub_filters competing for one anchor is a coin toss.
printf "sub_filter '</title>' '</title><style>%s</style>';\n" \
  "$desc{display:none}p:has(>$desc){display:none}$info{font-size:0}$info::after{font-size:.875rem;content:\"$line\"}" \
  > "$inc"

exec nginx -g 'daemon off;'
