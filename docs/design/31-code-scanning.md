# 31 - Code Scanning

> **Consumed by:** [30](30-continuous-delivery.md)
> **Status:** implemented. The scanners are in `.github/workflows/security.yml`;
> what they are pointed at is in `.github/codeql/codeql-config.yml`.

---

## 1. The problem a scanner creates

CodeQL with `security-extended` finds real bugs in this repository. It also
reports, at HIGH, that a field called `PasswordKey` reaches a log line - when
that field holds the *name* of a key inside a mounted Secret and never its
value.

Both kinds of finding arrive in the same tab, with the same badge, and the
count is the number somebody quotes in a meeting. A backlog that is mostly
noise is a backlog nobody reads, and the finding that mattered goes out with
the tide. So this document does two things: it says which findings this
repository acts on, and it records - with the evidence - which recurring ones
it does not, so the judgement is made once instead of at every scan.

**A finding is noise only when somebody has shown it cannot happen.** The
sections below carry that proof, not an opinion.

## 2. What the scanners are

| Scanner | Looks at | Gates a merge |
|---|---|---|
| CodeQL (`security-extended`) | Go, the web tier, the workflows | yes |
| govulncheck | Go advisories, filtered by call graph | yes |
| gitleaks | the whole history | yes |
| Trivy | base images, Dockerfiles, the tree | no - reported |
| pnpm audit / dependency review | the web tier's dependencies | dependency review only |

The gate is in the `security` job: only the scanners whose findings are
actionable on the diff in front of you can turn a pull request red. Trivy's
database and the npm advisory feed move on their own schedule, and a new CVE
published overnight must not fail a pull request that touches the chart.

## 3. What is scanned, and what is not

`.github/codeql/codeql-config.yml` excludes two kinds of tree. Neither is
shipped, and both would otherwise report findings that are the file working as
intended:

- **Compliance fixtures** (`internal/compliance/baseline/testdata`). The
  baseline checks are run against these. `bad-config.yaml` holds a Secret with
  an empty database password because `SEC-*` has to fail on something; an alert
  saying so is the fixture doing its job.
- **Test doubles** (`test/`). A fake Artifactory, a mock identity provider,
  seed data. `fakeregistry` hashes its fake passwords with SHA-256 precisely
  because it is a stub and not a password store.

Everything else is scanned, including `deploy/`. The seeder scripts run against
a real identity provider with real credentials and are exactly where a finding
would matter.

**`paths-ignore` does not cover Go.** It is applied when the extractor walks a
tree, and Go is extracted by building it - `autobuild` compiles every package
whatever this file says. Reproducing the scan locally with the config applied
drops the five fixture findings in YAML and JavaScript and leaves
`test/fakeregistry`'s untouched. So for Go the exclusions above are a statement
of intent that the tooling does not enforce, and a finding in a Go test double
is dismissed against the alert like any other in section 5. Do not read the
list as proof that a Go path was skipped.

**What would change our mind:** an exclusion that ever hides a finding in code
that reaches a binary or a cluster. If a fixture directory starts holding
anything that ships, it comes off this list rather than growing an exception.

## 4. Findings acted on

| Finding | Where | What was wrong |
|---|---|---|
| `actions/unpinned-tag` x6 | the three workflows | Third-party actions were pinned to a moving tag. `v6` is a branch somebody else can move; a compromised tag runs in a job holding `REGISTRY_TOKEN`. Now pinned to commit SHAs, with the tag in a trailing comment so a reader still knows what version it is. |
| `js/regex/missing-regexp-anchor` | `deploy/zitadel/bootstrap.mjs` | `/login\.microsoftonline\.com/.test(base)` searched anywhere in the issuer URL, so `https://attacker.example/login.microsoftonline.com` and `https://login.microsoftonline.com.attacker.example` both matched. The host is now parsed and compared. |
| `go/unsafe-quoting` x2 (CRITICAL) | `internal/registry/artifactory/xray.go` | The AQL criteria were assembled from separately-quoted fragments. `quoteAQL` did escape correctly, so this was not exploitable - but a reader had to prove that about every fragment, and the next field added to the object would not have had to be escaped by anybody in particular. The whole criteria object is now one `json.Marshal`. |

Dependabot keeps updating these: given `@<sha> # v6` it rewrites both the SHA
and the comment, so pinning costs nothing in upgrade hygiene and the pull
requests it opens read the same as the ones already in the history.

The unsafe-quoting entry is the pattern to copy. The alert was not a live
vulnerability; the code was still the wrong shape, and the fix is the shape
that cannot be got wrong later.

## 5. Findings classified as noise, with the proof

### 5.1 `go/clear-text-logging` x9 (HIGH) - `PasswordKey` is a key name

Every one of the nine traces back to `SecretResolver.Value` in
`internal/product/secrets.go`, whose errors name the secret and the key that
could not be read. `CredentialsRef.PasswordKey` is the **name** of a key inside
a mounted Secret - it defaults to the literal string `"password"` - and the
resolved value never enters an error or a log line. CodeQL classifies the field
by its name.

**What would change our mind:** a `Secret` value, or `Credentials.Password`,
appearing in a format string. The `Secret` type exists to make that visible:
it has to be `Reveal()`ed first.

### 5.2 `go/log-injection` x36 (MEDIUM) - the handler escapes the value

Every sink is a structured `slog` attribute under a constant message -
`q.log.WarnContext(ctx, "could not record worker", "worker", id, ...)`. Both
handlers this product builds (`log.go` picks text or JSON) escape control
characters in attribute values. Logging `"abc\nlevel=ERROR msg=\"forged\""`
produces:

```
time=... level=WARN msg="registry request failed" worker="abc\nlevel=ERROR msg=\"forged\" user=admin"
{"time":"...","level":"WARN","msg":"registry request failed","worker":"abc\nlevel=ERROR ..."}
```

The newline is a literal `\n` inside a quoted value in both. No second entry
can be forged. CodeQL does not model the handler.

**What would change our mind:** a log handler that is not one of the two stdlib
handlers, or any user-controlled value reaching a log through `fmt.Sprintf`
into the *message* rather than an attribute.

### 5.3 `go/weak-sensitive-data-hashing` (HIGH) - a fingerprint, not a password store

`hashCredentials` in `internal/replication/desired.go` exists so a credential
*rotation* is detectable without the credential being stored. It is never
compared against a user-supplied password. A salted, computationally expensive
KDF is the wrong tool twice over: the salt would make every reconcile produce a
different digest and report permanent drift - the exact failure the
`sync_start_date` comment above it already warns about - and the cost would be
paid on every reconcile.

**Worth knowing:** the digest is unsalted SHA-256 held in the database, so it is
open to an offline dictionary attack if that table leaks. An HMAC under a
deployment-held key would close that and stay deterministic. It is not done
here because it needs a key-management surface this design does not have, and
rotating into it reports drift once for every product. Revisit it when the
coordinator has a deployment secret for other reasons.

### 5.4 `js/clear-text-logging` x7 (HIGH) - a secret's name and a username

`K8S_STATE_SECRET` holds the **name** of the Kubernetes Secret to write, and is
logged so a failed run says which object it was reaching for.
`BOOTSTRAP_ADMIN_USERNAME` is a username. `BOOTSTRAP_ADMIN_PASSWORD` is read
into `pw` and is never logged: the seeder prints whether a password was set,
never the value, and names the variable when telling an operator to unset it.

### 5.5 `js/empty-password-in-configuration-file` (HIGH) - a default meaning "unset"

`bootstrapAdmin.password: ""` in the chart's `values.yaml` is the default that
says no local administrator password is configured, and the seeder refuses to
run with it set while `sso.issuer` is. The chart holds every default
([CLAUDE.md](../../CLAUDE.md)); an empty one is how "not set" is spelled.

### 5.6 `js/user-controlled-bypass` (HIGH) - the guard fails closed

`completeSignIn` throws when the issuer returns `?error=`. CodeQL sees a
user-controlled condition in front of the token exchange. The condition only
ever *adds* a rejection: every path that does not throw still checks `code`,
still requires the pending record this tab wrote, and still compares `state`
against it. An attacker controlling `error` can make sign-in fail, not succeed.

### 5.7 `js/file-access-to-http`, `js/http-to-file-access` (MEDIUM) - in-cluster identity

`k8s-state.mjs` reads the ServiceAccount token from
`/var/run/secrets/kubernetes.io/serviceaccount` and presents it to the API
server, then writes the API server's response to disk. That is what in-cluster
authentication is. The "untrusted" source is the kubelet.

## 6. How a finding is dismissed

In the Security tab, with a reason, against the alert. Not with a suppression
comment: this repository's comments say what a reader cannot see
([CLAUDE.md](../../CLAUDE.md)), and `// codeql[...]` scattered through
`queue.go` tells a reader about a scanner rather than about the queue. The
reasoning that belongs in the repository is in section 5 of this document,
which is one place to update when a classification stops being true.

**What would change our mind:** a recurring finding on code that changes often,
where the dismissal has to be re-applied on every new line. That is an argument
for fixing the shape of the code, as section 4 did.
