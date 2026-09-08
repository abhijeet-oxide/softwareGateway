# npm build credentials

`npmrc.default` is **zero bytes, and must stay that way.** It is the file the
`npmrc` build secret mounts when no credentials are configured, and its size is
load-bearing:

podman's remote client (podman machine, so every Windows and macOS host) cannot
pass a build secret to the server over the wire. It copies the secret INTO THE
BUILD CONTEXT, keeps the handle open, and tars the context - and on Windows the
size the tar header gets is the one the directory entry reports, which is still
zero while podman's writes sit unflushed. The copy then delivers the real bytes
and `archive/tar` refuses them:

```
archive/tar: write too long
Error: Post "http://d/v5.5.2/libpod/build?...": io: read/write on closed pipe
```

A zero byte secret is immune, because zero is what the header promised. This
file used to carry ten lines of comments explaining that it was empty, and
those comments were enough to break every build on Windows. See
containers/podman#26914, #17899 and #23815; `deploy/deploy_test.go` fails if
either default file grows again.

## Using an authenticated registry

Copy `npmrc.example` to `npmrc`, fill in the credentials, and set in `.env`:

```
NPM_CONFIG_FILE=./deploy/npm/npmrc
```

`deploy/npm/npmrc` is gitignored. Note that a credentialed npmrc is by
definition not empty, so under podman on Windows it hits the bug above until
podman fixes it. `deploy/STACK.md` lists what to do about that.
