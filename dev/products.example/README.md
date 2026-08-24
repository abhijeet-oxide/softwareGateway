# Example product configuration

Three vendor products pointed at `dev/fakeregistry`, which serves their release
trees locally over TLS. `dev/seed/up.sh` copies these into `dev/products/` when
that directory is empty, so a fresh clone gets a working estate without anybody
writing YAML first.

`dev/products/` itself is gitignored: it is where a developer's own
configuration goes, and a demo file overwriting it would be a surprise.

| Product | Source | Releases |
|---|---|---|
| `mavenir-core` | `registry.mavenir.example.com:9443/mavenir/converged-core` | 5, twelve components |
| `ericsson-ran` | `registry.ericsson.example.com:9443/ericsson/cloud-ran` | 4, six components |
| `nokia-cmm` | `registry.nokia.example.com:9443/nokia/cmm` | 3, five components |

All three replicate to one JFrog-flavoured target with Xray switched on, which
is what makes the vulnerability pages reachable. They set
`network.tls.insecureSkipVerify` because the development registry issues its own
certificate; nothing outside `dev/` does.
