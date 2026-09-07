# Extra CA certificates

Drop any `.crt` files here that your network's TLS-intercepting proxy uses, and
the images will trust them at build time. Most corporate networks need this;
without it `npm ci` and `go mod download` fail with
`SELF_SIGNED_CERT_IN_CHAIN` or `x509: certificate signed by unknown authority`.

The directory is committed empty on purpose: the Dockerfiles `COPY` it
unconditionally, and Docker cannot conditionally copy a file that may not
exist.

Files here are gitignored. Never commit a private key.
