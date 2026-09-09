# Example products

Three vendor products pointed at `test/cmd/fakeregistry`, which serves their
release trees locally over TLS. `test/seed/up.sh` reads them IN PLACE, through
`SWGW_PRODUCTSDIR`, so a seeded demo estate never writes into `data/products` -
the directory that describes what a real deployment replicates.

They are also the richer half of `task validate`: credentials, promotion
targets and compliance packs that the shipped `data/products` samples
deliberately leave out, because a validator only ever exercised against the
simple case is a validator nobody has tested.


| Product | Source | Releases |
|---|---|---|
| `mavenir-core` | `registry.mavenir.example.com:9443/mavenir/converged-core` | 5, twelve components |
| `ericsson-ran` | `registry.ericsson.example.com:9443/ericsson/cloud-ran` | 4, six components |
| `nokia-cmm` | `registry.nokia.example.com:9443/nokia/cmm` | 3, five components |

All three replicate to one JFrog-flavoured target with Xray switched on, which
is what makes the vulnerability pages reachable. They set
`network.tls.insecureSkipVerify` because the development registry issues its own
certificate; nothing outside `dev/` does.
