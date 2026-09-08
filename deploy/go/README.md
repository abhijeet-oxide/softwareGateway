# Go module proxy credentials

`netrc.default` is **zero bytes, and must stay that way** - see
[../npm/README.md](../npm/README.md) for why the size of a build secret's
default file decides whether every build on Windows works.

## Using an authenticated module proxy

Copy `netrc.example` to `netrc`, fill in the credentials, and set in `.env`:

```
GO_NETRC_FILE=./deploy/go/netrc
```

`deploy/go/netrc` is gitignored.

Do NOT put credentials in `GOPROXY` instead. That is a build argument: it is
recorded in the build request, printed in full in any error the build reports,
and lands in the build cache. A netrc is mounted for one `RUN` and touches no
layer.
