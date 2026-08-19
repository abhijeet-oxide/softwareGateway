# 08 - Verification

> **Prerequisites:** [02 - Configuration](02-configuration.md) §4, [06 - Registry Abstraction](06-registry-abstraction.md)
> **Feeds:** [ADR-001](16-technology-choices.md#adr-001) - §3.3 of this document is a scored input to the M3 library decision.

---

## 1. Two different guarantees

These are routinely conflated and must not be.

| | Digest verification | Signature verification |
|---|---|---|
| Proves | The bytes are the bytes we asked for | The vendor vouched for these bytes |
| Against | Corruption, truncation, a misbehaving registry or proxy | Tampering, substitution, a compromised registry |
| When | Inline, during every transfer ([05](05-transfer-engine.md) §4.4) | At explicit points (§4) |
| Cost | ~free, overlapped with I/O | A few RPCs per artifact |
| Configurable | No - always on | Yes, per product |

Digest verification is unconditional and cheap, so it is never a policy question. Signature verification is a policy question, and this document is about that.

## 2. Scope

**Cosign / Sigstore only in v1.** Keyed and keyless, discovered via the OCI 1.1 referrers API with a fallback to cosign's tag schema.

> **Decision - cosign/Sigstore, with Notary Project explicitly out of scope for v1.**
>
> *Alternative:* support `notation` as well, selected per product, behind a common interface.
>
> *Chosen:* cosign alone. It is what the cloud-native ecosystem overwhelmingly uses, and supporting two signing stacks means two trust-policy configuration shapes, two verification code paths, two dependency trees, and two sets of failure modes to explain to operators - for a second format we have no confirmed vendor requiring.
>
> *Cost accepted:* a vendor that signs exclusively with Notary Project cannot be verified. Such a product runs with `verification.enabled: false` and the gap is visible rather than silently unverified - `softwaregateway_packages_unverified` ([12](12-observability-and-audit.md) §2) counts them.
>
> *The seam:* §6 defines the `Verifier` interface. Adding `notation-go` is one implementation plus a config block, not a refactor.

## 3. Discovering signatures

A cosign signature is itself an OCI artifact in the registry, associated with the subject manifest by one of two conventions.

### 3.1 Referrers API (preferred)

```http
GET /v2/<name>/referrers/<subject-digest>?artifactType=application/vnd.dev.cosign.simplesigning.v1%2Bjson
```

OCI 1.1. Returns an index of artifacts referring to the subject. Correct, spec-defined, and does not pollute the tag namespace.

### 3.2 Tag schema (fallback)

Cosign's original convention: the signature for `sha256:abc…` lives at tag `sha256-abc….sig` in the same repository. Used when `SupportsReferrersAPI` is false ([06](06-registry-abstraction.md) §3).

Both are handled behind `Repository.Referrers`, so the rest of verification does not know or care which was used.

### 3.3 Input to ADR-001

> The M3 spike must assess **how much of §3.1–3.2 we implement ourselves versus inherit from cosign's registry packages.**
>
> This is not an idle question - it is the condition that most weakens the strongest argument for `go-containerregistry` ([16](16-technology-choices.md#adr-001)). That argument is that cosign's registry-facing packages are GGCR-typed, so choosing `oras-go` for transfer means carrying two OCI type systems and converting at every verification boundary.
>
> If signature *discovery* is hand-rolled against `Repository.Referrers` - which is roughly the two mechanisms above, both of which we already need the interface to expose - then the only cosign dependency left is **bundle and certificate verification**, which is `sigstore-go`'s domain and is largely registry-agnostic. In that case the type-system argument mostly evaporates and the decision turns on the other criteria.
>
> The spike must therefore produce a concrete answer to: *what is the smallest cosign/sigstore surface that gives correct keyless verification, and does it touch registry types at all?* Record the finding in the ADR closure.

## 4. When verification runs

| Stage | Trigger | Purpose | On failure (`enforce`) |
|---|---|---|---|
| **Source** | Before transfer, if `atSource` | Do not spend 45 GB of bandwidth on something we will reject | Transfer never starts; `failed` with reason |
| **Destination** | After transfer, if `atDestination` | Prove what landed is what was signed | Transfer → `failed`; **artifacts are left in place** |
| **On demand** | `transferctl verify` / API | Audit, incident response, periodic re-attestation | Reported; nothing changes |

Source verification is a genuine optimization as well as a control: rejecting early saves the transfer entirely.

Destination verification is the one that actually matters for trust, because it is the only check that covers our own transfer path. Source verification proves the vendor's copy is good; destination verification proves *ours* is.

> **On failure we do not delete.** The artifacts stay at the destination. Reasons: the tag was never applied (invariant I1), so nothing is exposed to consumers; the blobs may be legitimately shared with other packages, so deleting them could break something unrelated; and an operator investigating a verification failure needs the evidence, not a clean scene. The package sits in `verification_failed` and a notification fires.

**Policy** is `enforce` or `warn` ([02](02-configuration.md) §4). `warn` records the failure, notifies, and proceeds - appropriate while onboarding a vendor whose signing setup is not yet understood, and a state that `softwaregateway_verification_policy_warn` makes visible so it does not become permanent by accident.

## 5. Transferring signatures

With `transferSignatures: true` (default), signature artifacts are included in the transfer plan as ordinary artifacts.

This matters more than it first appears. Without it, the destination holds unsigned copies: an internal consumer pulling from our registry has **no way to verify anything**, and the chain of custody ends at our boundary. With it, the destination is independently verifiable by anyone, using the vendor's own trust policy, without trusting us or this tool.

Mechanically it is free - signatures are small OCI artifacts and move through the same engine as everything else. The planner resolves referrers during the manifest walk ([05](05-transfer-engine.md) §3) and adds them to the artifact set.

## 6. The Verifier interface

```go
type Verifier interface {
    // Verify checks all signatures on subject against policy.
    // Returns a per-artifact result set even on failure, so a report can name
    // exactly which image failed rather than just "the package".
    Verify(ctx context.Context, repo registry.Repository,
           subject digest.Digest, policy Policy) (Result, error)

    Name() string   // "cosign"
}

type Result struct {
    Verified   bool
    Artifacts  []ArtifactResult   // per-artifact outcome
    Signatures int
    Errors     []error
}
```

Persisted to `verifications` ([03](03-persistence.md) §7) with per-artifact detail in `details JSONB`. **A package-level pass/fail is not sufficient output**: when a 40-image package fails, the operator needs to know which image, and a boolean does not tell them.

## 7. Trust configuration

Per product ([02](02-configuration.md) §4), because trust is a vendor property.

**Keyless** (the common case for vendors publishing from CI):

```yaml
cosign:
  mode: keyless
  keyless:
    certificateIdentity: 'https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main'
    certificateOidcIssuer: 'https://token.actions.githubusercontent.com'
```

Both fields are **required** in keyless mode, and this is enforced at config load. Keyless verification without an identity constraint accepts *any* valid Sigstore signature from *anyone* - it proves someone signed it, not that the vendor did. That is a trust configuration that looks secure and is not, so the loader rejects it rather than allowing it.

**Keyed:**

```yaml
cosign:
  mode: key
  key:
    publicKeyRef: {secretName: vendor-a-cosign, key: cosign.pub}
```

**Air-gapped / private Sigstore.** Rekor public keys, Fulcio roots, and a TUF root can be supplied from Secrets ([02](02-configuration.md) §4), so verification works without reaching the public Sigstore instances. The product's CA bundle applies to any Sigstore endpoint we do contact, through the shared transport ([06](06-registry-abstraction.md) §5).

## 8. Lifecycle

States: `pending → running → passed | failed | error | skipped` ([10](10-state-machines.md) §5).

`failed` and `error` are deliberately distinct, and the distinction is operationally important:

- **`failed`** - verification ran and the signature did not check out. This is a security event.
- **`error`** - verification could not run: Rekor unreachable, a malformed policy, a network fault. This is an availability event.

Collapsing them would mean a Rekor outage looks identical to a supply-chain attack. Under `enforce`, both block the transfer - but they page different people and imply different responses, so `error` retries with backoff while `failed` does not retry at all. Retrying a signature that definitively does not verify accomplishes nothing except making the alert repeat.

## 9. Idempotency

Re-verification is always safe and always allowed - it is a read-only operation against the registry plus a row insert. Each run writes a new `verifications` row rather than mutating the previous one, so verification history is preserved and "when did this last verify" is answerable. There is no dedupe key here on purpose: unlike transfers, running verification twice is not waste, it is evidence.
