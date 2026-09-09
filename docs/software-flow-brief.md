# Software Flow

**Scope:** Reference pattern for automated software ingestion, onboarding and promotion through the Software Gateway.

Sofware Gateway is a OCI artifact moving tool that is meant to discover software from Multiple sources to multiple targets.

We try to be as generic and OCI compliant and only deviate for vendor and repo type implementation. 

For example we have near from Nokia (which defines how the software ios packages as ORB, its content, its signature validation process etc).

Then we have Jfrog which is still OCI repo, but then Jforg has Xray capability, Build concept which is to be implement for repo type Jfrog.

Then we have Anchore which defines like a plugin that can be clubed in any repo.

This document defines one supported automated software flow. It is not the only supported pattern. The behaviour described here is entirely driven by product configuration, so other patterns can be expressed by changing the configuration alone.

All registries, repositories, secrets and e-mail addresses in this document are generic placeholders. Substitute real values at deployment time.

---

## 1. Overview

A software package moves through three environments, in a fixed order:

| Stage | Purpose | Repository (placeholder) | Entry method |
|---|---|---|---|
| `external` | Untrusted landing zone for vendor artifacts | `oci-external` | Automatic (auto-download rules) |
| `lab` | Validated artifacts available for lab deployment and testing | `oci-lab` | Manual (`Onboard to Lab`) |
| `prod` | Released artifacts approved for production | `oci-gold` | Manual (`Promote to Prod`) |

---

## 2. Flow

### 2.1 Configuration by the user in product config

1. The user defines one or multiple **source** (the vendor registry).
2. The user defines targets of env type external, lab and prod.
3. The user enables **auto-download** on the `external` stage only.

### 2.2 Automatic ingestion into `external`

Triggered by discovery of a new vendor tag. Steps run in order and stop on first failure:

Sample Config aqlong with requested changes.
```
sources:
    - name: vendor-repo
      type: near --> change this from vendor to type.
      registry: example-repo.com
      credentialsRef:
        secretName: repo-secret
      network:
        caBundleRef:
          secretName: repo-ca
          key: ca.crt
        proxy:
          httpsProxy: sample_proxy
      discovery:
        enabled: true
        interval: 15m
        maxRepositories: 10000
        repositoryFilters:
          include: ['^orbs/']
        tagFilters:
          include: ['^orb_']
          exclude: ['^orb_.*_base_.*$']
      verification: --> Move the verification defined at repo level. We would define how to validate.
        enabled: true
        policy: enforce --> If we should fail auto download.
        atSource: true --> Nbot needed.
        atDestination: true --> Nbot needed.
        transferSignatures: true --> If we should donwload sign in download
        cosign: --> Method of veriofication.
          mode: keyless
          keyless:
            # Both constraints are REQUIRED. Without certificateIdentity, any valid
            # Sigstore signature would be accepted — proving someone signed the
            # artifact, not that the vendor did.
            certificateIdentity: 'https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main'
            certificateOidcIssuer: 'https://token.actions.githubusercontent.com'
      compliance:
        enabled: true
        policy: enforce --> If we should fail auto download. (enfore | warn)
      concurrency: --> this should be optional. Default is configured at coordinator for each product. Here it can be overidden.
        perRegistry: 16
        requestsPerSecond: 50
```
1. **Verify signature at source.** The artifact is verified in the vendor registry before any bytes are pulled.
2. **Run compliance checks.** defined if we should run compliance checks.
3. **Download into the `external` repository.**
4. **Replicate to Anchore** and index in Xray.

If step 1 or 2 fails, the artifact is not downloaded and a `VerificationFailed` notification is raised.

After completion of the stages configured user should be notified for New software avaiability with link of the package details page.


### 2.3 Review in the tool

The user opens the product in the tool and sees the onboarding status of each package. 
The timeline shows `Published` --> `Signature` --> `Compliance` --> `Download` --> `Security`. Each stage should show time, time taken where relevant like compliance, Signature, Download, Security. User should be able to expand and view details. Like Security should show Anchore Replication.
In the top right we also want a new `Logs` button (similar to sync logs). That opens timeline better with error, stage, time, and more details. Make ikt look and behave exactly like Sync logs button and side panel. With icons, colors, message etc.

Lets say auto download failed then show the message in top. reason. Error message.

If everything goes good and sw is downloaded drilling in shows the **Compliance** and **Security** tabs. Under **Security**, `Sync` pulls current findings from Xray and Anchore and renders them.

If the user accepts the findings, the primary action at the top of the page is **`Onboard to Lab`**. (We should not see Promote or download) (Donwload showuld be visible if the SW is not downloaded, promote if sw is in lab).

### 2.4 Onboard to Lab

`Onboard to Lab` copies the artifact from the `external` repository to the `lab` repository (Since its same registry its mosty a copy index anyway). Now we are to make a design choice here. After move to Lab action SW is deleted from External repo. So the Xray link breaks. we want sync action to always be able to pull latest findings and refresh result. Moving to lab must not break this. So if we sync again it must sync form the current repo. which would be lab now. Anchore would remain same. So sync would just pull again.

The timeline now adds a new item `Lab` wityh time and timetaken for download.

The compliance can also be run again. However dependeing on the location we must pull charts. Initialy it was from vendor repo. If downlaoded then from external, if in lab then from Lab repo. and so on.

### 2.5 Promote to Prod

`Promote to Prod` copies the artifact from `lab` to the `prod` (gold) repository. This is a promotion only. `prod` never accepts a direct transfer from the source or from `external`.

This adds new item in timeline `Production`.
---

---

## 4. Product configuration

As we mentioned before the configuration needs updating to simplify and imrpove it.

At high level we want each product to define source, target, notification. Things like concurency, timeout must bve defualted to config.yaml and only overwirtten at rpoduct level.

The schema must be generic enouigh to acomodate other paterns.

Any repo specific action must be done by specificying repo type as jfrog, near, etc.

Anchore Scan must be also default in config and overwritten in product. Use similar paterns everywhere for example secret ref for anchore must be like how we pass secret for repo. Same for certs. Also each connectibity must define a optional way to disable cert validation (lets say in lab).

I also want a way to define this flow. We already have a wrapper for download called promote that downlaods for lab to prod. We need onboard wrapper. from external to lab.

Also evalaute whats the best way to make this generic. How can we specifiy various steps like obnoard, download, promote. I think we should make this as config. Product can define sourcves and taregts and then define pipelines called download, onboard and promote. And their details like source and target. But then my concern is its repetetve. Evry product has to do this. What if we can define it config level and define repo env instead. Each product must define those env and then the pipelje flow is eastablished. The idea is many teams may use it and they may not have external repo lets say so for them auto download can be directly in lab. How we acomodate for that. We want the tool to be simple cofnigurable. Yet flexible for various deviations.

Also the UI shows downlaod and promote. If some product deifne a new flow how do we show it to them? There are so many ambiguity but understand this and suggest me the rightw ay. Dont be too verbose. Give me clear simple design and a sample config i can see and validate. When i apporove then iomeplement whole.