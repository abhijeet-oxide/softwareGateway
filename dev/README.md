# dev/ - local runtime state

Nothing here is configuration and nothing here is committed. The whole
directory is disposable: deleting it is a reset.

```
dev/swgw.db          the SQLite database `task run` writes
dev/swgw.db-shm      its journals
dev/swgw.db-wal
```

It used to hold rather more, and none of it belonged:

| was | is now | why |
|---|---|---|
| `dev/config.yaml` | `config/config.yaml` | one configuration for `task run`, compose and Flux |
| `dev/products.example/` | `test/products.example/` | a fixture the validator runs against |
| `dev/secrets/` | `test/secrets/` | fixtures for the seeded local estate |
| `dev/seed/` | `test/seed/` | the script that brings that estate up |
| `dev/fakeregistry/` | `test/cmd/fakeregistry/` | a command, and a test double |
| `dev/docker-compose.dev.yaml` | `deploy/dev/docker-compose.yml` | machinery, like every other compose file |
| `deploy/mock-entra/` | `test/mockEntra/` | a double, reunited with the compose file that runs it |
| `build/` | `deploy/build/` | Dockerfiles are shipping machinery, and `deploy/` already held the rest of it |
| `scripts/` | `deploy/scripts/` | one operator helper did not earn a root directory |
| `data/` | `config/` | it holds configuration, and `data` sat one letter from `db/` |

The line those moves draw: **`config/` is content an administrator manages,
`deploy/` is machinery, `test/` is what proves it works, and `dev/` is what one
laptop happens to have written.** Only the last is disposable, and only the last
is ignored.
