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

# WHO TO ASK, on the one screen that actually turns somebody away.
#
# A person the directory authenticated but this system does not know is refused
# INSIDE ZITADEL: they read its Account Not Found page and never reach the
# application, so nothing the product draws can speak to them. That page ends
# "contact your administrator for assistance", which names nobody.
#
# SUPPORT_CONTACT names somebody, and this proxy is already the only thing
# between that page and a browser. Unset, this writes an empty include and
# ZITADEL's own wording stands: an invented address is worse than a general
# sentence.
#
# DONE IN CSS, and that is not a shortcut. The obvious version - rewrite the
# sentence in the HTML with sub_filter - was written, deployed and watched: the
# server sends the new text, React hydrates over it, and the original sentence
# is back before anybody reads it. A stylesheet is not hydrated. So the
# paragraph is zeroed and its replacement is the ::after content, which costs
# the address being text rather than a mailto link and buys a rule that cannot
# be undone by the app and cannot execute anything.
#
# The anchor is the paragraph's i18n key, not its English text, so a deployment
# running in another language is matched too. If a later login image renames
# the key the rule stops matching and the page reads as it always did, which is
# cosmetic and not a broken sign-in.
inc=/etc/nginx/conf.d/support-contact.inc
: > "$inc"
if [ -n "${SUPPORT_CONTACT:-}" ]; then
  key='[data-i18n-key="idp.accountNotFound.info"]'
  line="No account on this system holds the address the directory provided. Request access from ${SUPPORT_CONTACT}."
  # </title> rather than </head>: the rule above already claims </head>, and
  # two sub_filters competing for one anchor is a coin toss.
  printf "sub_filter '</title>' '</title><style>%s{font-size:0}%s::after{font-size:.875rem;content:\"%s\"}</style>';\n" \
    "$key" "$key" "$line" > "$inc"
fi

exec nginx -g 'daemon off;'
