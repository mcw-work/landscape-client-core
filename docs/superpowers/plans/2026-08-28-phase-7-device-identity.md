# Phase 7 - Device Identity Feasibility Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to execute this research and protocol-design plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Determine whether Ubuntu Core serial assertions can support stronger Landscape enrollment, and produce an owner-approved protocol design or a precise blocker without changing production registration.

**Architecture:** The investigation separates assertion-chain attestation from proof of possession. It verifies the complete assertion format, model-authorized serial authority, account/account-key chain and snapd APIs; then defines server-side trust, replay, rotation, mixed-fleet and bearer-token behavior. Client implementation remains gated on a supported signing operation and server protocol approval.

**Tech Stack:** snapd assertion format and REST API, Ubuntu Core, Landscape message API 3.3, Go 1.25, TLS

**Spec:** [docs/superpowers/specs/2026-08-28-code-review-follow-up-design.md](../specs/2026-08-28-code-review-follow-up-design.md)

**Branch:** `research/07-device-identity`, cut from `fix/05-runtime-boundaries`; independent of Phase 6

---

## Non-negotiable Security Rules

- Never commit a real serial/model assertion, serial number, device key, account key not already public, registration key, `secure-id`, `insecure-id`, exchange token, nonce, or server response containing credentials.
- Never request or export snapd's device private key.
- Do not add a cryptography dependency during feasibility work.
- Do not describe snap assertions as OpenPGP. Verify the snapd assertion format and signature algorithms from pinned upstream source and official documentation.
- Forwarding a signed assertion is attestation, not proof that this client currently controls the asserted device key.
- Do not overload `accepted-types` as feature negotiation without a server protocol decision; it currently describes inbound message types, not registration capabilities.
- Do not weaken TLS or add automatic fallback from stronger authentication to a shared secret without an explicit downgrade policy.

## Required Owners and Environments

Identify named owners for:

- landscape-client-core;
- Landscape server registration/authentication;
- snapd device identity and assertions;
- snap confinement/interface security;
- brand/account key operations;
- an isolated Landscape staging server;
- a disposable Ubuntu Core test device with a serial assertion.

If the snapd and server owners are unavailable, the valid outcome is `BLOCKED`
with the unanswered questions recorded. It is not valid to infer an API contract.

## Decision Artifact

Create one artifact during execution:

`docs/superpowers/specs/2026-08-28-device-identity-decision.md`

Use this exact outline:

```markdown
# Device Identity Decision

**Evidence date:** YYYY-MM-DD
**Client baseline:** <commit>
**snapd revision/version:** <commit and device version>
**Server build/environment:** <non-secret identifier>
**Decision owners:** <names or teams>

## Decision
TELEMETRY_ONLY / APPROVE_ATTESTATION / APPROVE_PROOF_OF_POSSESSION / REJECT / BLOCKED

## Current Registration And Threat Model
## Serial And Model Assertion Evidence
## Trust Chain And Key Lifecycle
## Proof-Of-Possession Capability
## Proposed Protocol
## Negotiation And Downgrade Policy
## Credential Storage And Cloning
## Confinement Review
## Rollout And Rollback
## Open Questions
## Follow-up Plans
```

An approval requires named server, snapd and security owners with dates. Raw device
captures remain outside the repository; the record contains redacted structure,
hashes, versions and conclusions only.

## Task 0: Create the research branch

- [ ] **Step 1: Verify the implementation baseline**

```bash
git switch fix/05-runtime-boundaries
git status --short
go test -race ./internal/snapd ./internal/exchange ./integration
```

Expected: tests pass and no tracked changes. Do not remove the untracked August
review or planning documents.

- [ ] **Step 2: Create the branch**

```bash
git switch -c research/07-device-identity
```

## Task 1: Pin the current client registration contract

**Files inspected:**
- `internal/snapd/snapd.go`
- `internal/snapd/snapd_test.go`
- `internal/monitor/computerinfo.go`
- `internal/exchange/exchange.go`
- `internal/exchange/exchange_test.go`
- `internal/persist/persist.go`
- `docs/protocol.md`

- [ ] **Step 1: Locate every identity transition**

```bash
rg -n 'GetAssertions|fetchAssertion|parseAssertion|Serial|Brand|Model' internal/snapd internal/monitor
rg -n 'register|registration_password|secure-id|insecure-id|X-Computer-ID|X-Exchange-Token|accepted-types' internal/exchange internal/persist docs/protocol.md
```

Record the exact current behavior:

1. snapd serial/model assertions are fetched, but only selected headers survive;
2. `serial`, `model`, and `brand-id` are reported as telemetry;
3. registration uses account name plus shared registration password and
   unauthenticated machine ID;
4. successful registration yields cleartext bearer identifiers in `state.json`;
5. later exchanges authenticate with `X-Computer-ID` and optional rotating
   `X-Exchange-Token`;
6. copying valid state may clone client identity unless the server adds another
   proof.

- [ ] **Step 2: Verify existing regression coverage**

```bash
go test ./internal/snapd -list 'Test(GetAssertions|ParseAssertion)'
go test ./internal/exchange -list 'Test(Registration|.*SetID|.*Token)'
go test ./integration -list 'Test.*Registration'
go test ./internal/snapd -run '^TestGetAssertions$' -count=1
go test ./internal/exchange -run '^TestRegistration$' -count=1
```

If a regex matches no test, record that exact gap in the decision document; do
not rename tests or add speculative protocol tests during this task.

- [ ] **Step 3: Draw the current trust flow**

Add a Mermaid sequence diagram to the decision record showing snapd, client and
server for registration and subsequent exchanges. The diagram must distinguish:

- brand authorization from the model assertion and the serial assertion's actual authority;
- account/account-key chain used to verify that authority;
- TLS server authentication;
- shared registration secret;
- server-issued bearer credentials;
- absence of a device-key signature over a fresh server challenge.

## Task 2: Capture assertion structure safely on Ubuntu Core

- [ ] **Step 1: Record the device environment without identity values**

On the disposable device run:

```bash
snap version
uname -a
snap debug confinement
```

Record snapd version, Core series, architecture and confinement mode. Redact host
names and device serials.

- [ ] **Step 2: Capture full assertions outside the repository**

Use snapd's supported command first:

```bash
umask 077
snap known serial > /tmp/phase7-serial.assert
snap known model > /tmp/phase7-model.assert
sha256sum /tmp/phase7-serial.assert /tmp/phase7-model.assert
```

If `snap known` requires assertion selectors on that snapd version, obtain the
exact selector from `snap known --help` and record the command without its device
value. Do not copy either raw file into the workspace.

- [ ] **Step 3: Record redacted structural facts**

From each capture, record only:

- assertion type;
- authority ID redacted to a stable hash if non-public;
- presence and name of signing-key headers;
- body length and encoding;
- signature encoding and length;
- number and order of blank-line-delimited sections;
- SHA-256 of the complete capture;
- whether repeated retrieval is byte-identical.

Use commands that avoid printing identity values into shared logs:

```bash
wc -c -l /tmp/phase7-serial.assert /tmp/phase7-model.assert
awk 'BEGIN{section=1} /^$/{section++; next} {count[section]++} END{for (s in count) print "section",s,"lines",count[s]}' /tmp/phase7-serial.assert
cmp -s /tmp/phase7-serial.assert <(snap known serial) && echo byte-identical
```

- [ ] **Step 4: Delete raw captures after the record is complete**

```bash
shred -u /tmp/phase7-serial.assert /tmp/phase7-model.assert 2>/dev/null || \
  rm -f /tmp/phase7-serial.assert /tmp/phase7-model.assert
```

The hash is evidence of the inspected artifact, not a public test fixture.

## Task 3: Verify snapd trust and signing capabilities

- [ ] **Step 1: Pin an upstream snapd revision**

Use a clean external directory, not this repository:

```bash
gh repo clone snapcore/snapd /tmp/snapd-phase7
pushd /tmp/snapd-phase7
git rev-parse HEAD
git describe --tags --always
```

Select the revision corresponding to the device's snapd version as well as current
main if they differ. Record both hashes.

- [ ] **Step 2: Trace serial assertion creation and verification**

```bash
rg -n 'serial assertion|SerialType|device key|device-key|sign-key-sha3-384|authority-id' asserts overlord daemon client
rg -n 'CheckSignature|Signature|Sign\(' asserts overlord daemon client
rg -n 'assertions/serial|assertions/model' daemon client
```

Record which component owns the private key, how the serial assertion binds its
public key, how account keys are resolved, and which library the server would need
to reproduce trust-chain verification.

- [ ] **Step 3: Search every supported API for signing on behalf of the device**

```bash
rg -n 'sign.*(data|message|nonce|challenge)|device.*sign|Sign\(' daemon overlord client cmd interfaces
rg -n 'POST.*assert|/v2/assert|system-key|device-key' daemon client docs
```

For every plausible endpoint, record request schema, authorization, confinement
requirements, version introduced, and whether it signs an arbitrary caller
challenge with the key bound by the serial assertion.

Absence from one grep is not proof. Have the snapd owner confirm one of:

- a supported signing API already exists;
- a new narrowly scoped challenge-signing API is acceptable;
- exposing such signing is rejected.

- [ ] **Step 4: Verify confinement implications**

With the snapd/security owner, determine whether the existing snapd socket access
permits the proposed operation, whether a new interface is needed, which snap may
call it, and how snapd prevents a different confined snap from impersonating the
device to Landscape.

Record a security-owner decision. Do not prototype by broadening plugs or using
classic confinement.

- [ ] **Step 5: Clean external source**

After recording pinned hashes and conclusions:

```bash
popd
rm -rf /tmp/snapd-phase7
```

Expected: the shell returns to the landscape-client-core repository before the
temporary clone is removed. Later relative paths and Git commands must run there.

## Task 4: Define server trust and lifecycle requirements

- [ ] **Step 1: Assign trust-chain ownership**

The server owner must specify:

- how model-brand and delegated serial-authority account keys are obtained and authenticated;
- how `serial-authority` delegation in the model assertion is enforced;
- which snap assertion library or service verifies signatures;
- key rotation, expiry and revocation behavior;
- whether non-Canonical brands are supported;
- audit evidence retained for a registration decision.

The client should transport complete assertion bytes losslessly; it should not
become the authority that declares its own assertion trusted.

- [ ] **Step 2: Define proof-of-possession transcript**

If snapd can sign a challenge, specify a canonical transcript containing at least:

```text
protocol-version
server-origin
authentication-session-id
account-name
device-serial-assertion-digest
server-nonce
issued-at
expires-at
```

Define exact byte encoding, hash and signature algorithms, nonce entropy, maximum
lifetime, replay cache key, clock-skew policy, and one-time-use behavior. Ambiguous
JSON serialization is not acceptable unless a canonical JSON profile is named.

- [ ] **Step 3: Decide the value of attestation without possession proof**

Choose and document one:

- `REJECT`: assertion forwarding without proof does not strengthen enrollment;
- `TELEMETRY_ONLY`: server verifies and records provenance but still requires the
  shared registration password;
- `APPROVE_ATTESTATION`: verified brand issuance changes authorization policy even
  without current possession proof.

Do not describe attestation-only enrollment as asymmetric client authentication.

- [ ] **Step 4: Define credential behavior after enrollment**

Decide whether subsequent requests remain bearer-token authenticated. If yes,
state explicitly that stronger onboarding does not prevent cloning a copied
`state.json`. If no, define per-exchange signing or token binding, rotation,
recovery after lost state, and server revocation behavior.

## Task 5: Design compatibility and downgrade behavior

- [ ] **Step 1: Choose a protocol negotiation mechanism**

Evaluate these options with the server owner:

1. an explicit server capability returned before registration;
2. a new versioned registration message with a typed unsupported response;
3. operator configuration requiring asymmetric enrollment for selected fleets.

Do not infer capability from `accepted-types` unless the server formally expands
that field's contract beyond inbound command types.

- [ ] **Step 2: Define mixed-fleet policy**

Specify behavior for:

- old client/new server;
- new client/old server;
- Core device without serial assertion;
- device with unknown/revoked brand key;
- snapd without challenge signing;
- transient signing failure;
- rejected or replayed nonce;
- server rollback after clients upgrade.

For each case, say whether registration fails closed, requires operator opt-in to
legacy registration, or safely falls back. Silent automatic downgrade is not an
acceptable default.

- [ ] **Step 3: Define rollout and rollback signals**

Name metrics and logs for assertion fetch, trust-chain verification, challenge
issuance, signing, replay rejection, enrollment outcome, and downgrade. Define
thresholds that pause rollout and how a device recovers without remote shell
access.

## Task 6: Select an outcome and write the protocol

- [ ] **Step 1: Apply the outcome gate**

Choose exactly one:

- `APPROVE_PROOF_OF_POSSESSION`: snapd exposes/accepts a confined signing API,
  server protocol and trust lifecycle are approved, and mixed-fleet behavior is
  defined;
- `APPROVE_ATTESTATION`: server approves a clearly scoped assertion-verification
  feature that does not claim possession proof;
- `TELEMETRY_ONLY`: the server verifies and records assertion provenance while
  retaining the shared registration password as the authorization decision;
- `REJECT`: cost or threat-model benefit does not justify the feature;
- `BLOCKED`: name the missing owner, API, environment or decision.

- [ ] **Step 2: Complete the decision artifact**

Create `docs/superpowers/specs/2026-08-28-device-identity-decision.md` with the
required outline. Include sequence diagrams for current and proposed flows, exact
protocol fields if approved, and a table mapping each threat to its mitigation or
residual risk.

- [ ] **Step 3: Validate the artifact**

```bash
rg -n '^## (Decision|Current Registration|Serial And Model|Trust Chain|Proof-Of-Possession|Proposed Protocol|Negotiation|Credential Storage|Confinement|Rollout|Open Questions|Follow-up)' docs/superpowers/specs/2026-08-28-device-identity-decision.md
git add -N docs/superpowers/specs/2026-08-28-device-identity-decision.md
awk '
  /^## Decision$/ { in_decision=1; next }
  /^## / && in_decision { in_decision=0 }
  in_decision && /^(TELEMETRY_ONLY|APPROVE_ATTESTATION|APPROVE_PROOF_OF_POSSESSION|REJECT|BLOCKED)$/ { count++ }
  END { if (count != 1) exit 1 }
' docs/superpowers/specs/2026-08-28-device-identity-decision.md
git diff --check -- docs/superpowers/specs/2026-08-28-device-identity-decision.md
rg -n 'registration_password|secure-id|insecure-id|X-Exchange-Token' docs/superpowers/specs/2026-08-28-device-identity-decision.md
```

Expected: all required sections exist and exactly one outcome is selected. Review
every final grep match manually: it may name protocol fields, but must not include
a credential value. Automated text matching cannot prove secret absence.

- [ ] **Step 4: Obtain explicit review**

Record dated approval or objection from client, server, snapd and security owners.
Resolve contradictions in the artifact. Unknowns remain under `Open Questions`
and force `BLOCKED` if they affect authentication correctness.

## Task 7: Produce outcome-specific follow-up plans

- [ ] **Step 1: Plan client work only for an approved protocol**

If attestation is approved, the implementation plan may include:

- a raw assertion result type in `internal/snapd` that preserves exact bytes;
- tests with synthetic/non-sensitive assertions;
- registration payload fields named by the approved server contract;
- maximum assertion sizes and malformed-response handling;
- feature negotiation and downgrade tests;
- integration tests against the staging server contract.

If proof of possession is approved, additionally plan:

- a narrow snapd signing client method matching the approved API;
- canonical transcript construction;
- nonce expiry and replay tests;
- confinement declaration changes approved by snapd/security;
- staged server-first rollout and failure recovery.

Write the local client plan to
`docs/superpowers/plans/2026-08-28-device-identity-client.md` and link it from the
decision artifact. Do not create this plan for `REJECT` or `BLOCKED`.

- [ ] **Step 2: Keep server and snapd work in their owning repositories**

Create linked plans/issues in those repositories for trust verification, nonce
state, signing API, policy and rollout. Do not place pseudocode for unowned server
or snapd changes into this client as if it were implementable here.

- [ ] **Step 3: Stop cleanly for `TELEMETRY_ONLY`, `REJECT`, or `BLOCKED`**

For `TELEMETRY_ONLY`, plan only the approved provenance telemetry and retain the
shared-secret authorization path. For `REJECT`, record why no implementation
follows. For `BLOCKED`, create only owner-assigned investigation issues and the
condition that reopens planning. Do not add dormant client fields, crypto
dependencies, feature flags or dead code.

- [ ] **Step 4: Validate and commit the complete research outcome**

```bash
git add -N docs/superpowers/specs/2026-08-28-device-identity-decision.md
if test -e docs/superpowers/plans/2026-08-28-device-identity-client.md; then
  git add -N docs/superpowers/plans/2026-08-28-device-identity-client.md
fi
git diff --check -- docs/superpowers/specs/2026-08-28-device-identity-decision.md docs/superpowers/plans/2026-08-28-device-identity-client.md
git add docs/superpowers/specs/2026-08-28-device-identity-decision.md
if test -e docs/superpowers/plans/2026-08-28-device-identity-client.md; then
  git add docs/superpowers/plans/2026-08-28-device-identity-client.md
fi
git commit -m "docs: decide serial-assertion onboarding"
```

Expected: the decision and any required local implementation plan are tracked in
one commit, and the decision links to owner-repository work for server or snapd.
