# A local deployment with data in it

```sh
dev/seed/up.sh          # fresh database, discovery, transfers, dressing
dev/seed/up.sh --keep   # leave the existing database alone
cd web && pnpm dev      # http://localhost:5173
```

Three processes, no Docker, no Postgres, no cluster: the development registries,
the Coordinator and one Worker.

## What is real

Almost all of it. `dev/fakeregistry` wraps `test/fakeregistry` - the same
in-process OCI Distribution server the test suite runs against - in a TLS
listener per vendor hostname, seeded with release trees. From there the ordinary
system runs: discovery walks the tag lists and records the artifact trees, the
auto-download rule fires, the Coordinator plans the transfers and the Worker
streams the blobs registry to registry. Nine transfers actually move bytes.

`/etc/hosts` gets one line per vendor hostname, added by `up.sh`, so the
products can name `registry.mavenir.example.com` rather than a port on
localhost.

## What is dressed in afterwards

`dress.py` writes the two things a laptop cannot produce:

- **scanner findings** - there is no Xray in the loop, and the vulnerability
  pages are most of what this database exists to exercise;
- **signatures** - there is no cosign either, and an unsigned estate hides the
  verification column entirely.

## Sizes, and the one compromise

Releases have to weigh what real ones weigh - a 23 GB packet core is the point
of this system - and a fake registry that really served 23 GB would need 23 GB
of memory. A registry that only *claimed* to breaks the push, because the Worker
sets `Content-Length` from the descriptor.

So `up.sh` does both halves in order: it seeds and transfers with honest
kilobyte sizes, then restarts the registries with `-inflate` once the bytes have
moved. Every page then agrees on what a release weighs, including the release
comparison, which reads the registry live rather than the database.

The cost is stated where it bites: after the inflate restart a **new** transfer
will fail. Re-run `up.sh` to move bytes again.

## Files

| Path | What it does |
|---|---|
| `up.sh` | Brings the whole thing up, in order, and waits for each stage |
| `dress.py` | Writes scanner findings, signatures and display sizes |
| `../fakeregistry/` | The vendor and destination registries |
| `../products.example/` | The three products, copied into `dev/products/` |
