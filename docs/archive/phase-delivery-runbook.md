# Phase Delivery Runbook

## Purpose

Use this runbook to review, merge, and close Phases 0-7 safely.

The detailed plans explain implementation. This runbook explains delivery:

- what to check;
- when to stop;
- how to merge the stacked branches;
- how to complete the Phase 6 and 7 decisions.

## Rules

1. Work from the lowest phase upward.
2. Use a merge commit for stacked PRs. Do not squash or rebase them while stacked.
3. Do not delete a phase branch until its child PR targets `main`.
4. Run local checks on every stacked PR. Current CI runs only for PRs targeting `main`.
5. Stop on a real failure. Do not merge first and investigate later.
6. Do not turn missing research evidence into approval. Use `DEFER` or `BLOCKED`.
7. Never commit credentials, device identities, assertions, or private server data.
8. Review and test the exact PR commit, not a possibly stale local branch.

The merge-commit rule preserves ancestry. This lets each child PR become a clean,
phase-only diff when it is retargeted to `main`.

## Current Phase Map

| Phase | Branch | PR | Initial base | Purpose | Detailed plan |
|---|---|---:|---|---|---|
| 0 | `fix/00-foundation` | #4 | `main` | Make CI and build inputs reliable | [Phase 0 plan](superpowers/plans/2026-08-17-phase-0-foundation.md) |
| 1 | `fix/01-p0-defects` | #5 | `fix/00-foundation` | Fix critical runtime defects | [Phase 1 plan](superpowers/plans/2026-08-17-phase-1-p0-defects.md) |
| 2 | `fix/02-p1-reliability` | #6 | `fix/01-p0-defects` | Add timeouts, state safety, and liveness | [Phase 2 plan](superpowers/plans/2026-08-17-phase-2-p1-reliability.md) |
| 3 | `fix/03-p1-efficiency` | #7 | `fix/02-p1-reliability` | Fix queueing, scheduling, and allocation | [Phase 3 plan](superpowers/plans/2026-08-17-phase-3-p1-efficiency.md) |
| 4 | `fix/04-p2-hygiene` | #3 | `fix/03-p1-efficiency` | Finish logging, lint, tests, and cleanup | [Phase 4 plan](superpowers/plans/2026-08-17-phase-4-p2-hygiene.md) |
| 5 | `fix/05-runtime-boundaries` | #8 | `fix/04-p2-hygiene` | Bound dispatch and correct polling | [Phase 5 plan](superpowers/plans/2026-08-28-phase-5-runtime-boundaries.md) |
| 6 | `research/06-protocol-semantics` | New PR | `main` after Phase 5 | Decide wire-visible semantics | [Phase 6 plan](superpowers/plans/2026-08-28-phase-6-protocol-semantics.md) |
| 7 | `research/07-device-identity` | New PR | `main` after Phase 5 | Decide device identity design | [Phase 7 plan](superpowers/plans/2026-08-28-phase-7-device-identity.md) |

PR numbers are the current repository state. Confirm them before use:

```bash
gh pr list --state open --json number,title,headRefName,baseRefName,url
```

## 1. Prepare The Workstation

### Step 1: Install the required tools

```bash
go version
git --version
gh --version
golangci-lint version
govulncheck -version
rg --version
```

Expected:

- Go satisfies `go.mod`;
- golangci-lint is v2.12.2 or the version pinned by CI;
- every command exits successfully.

**Purpose:** Local checks must use the same major tool versions as CI.

**Stop if:** A tool is missing or uses an incompatible version.

### Step 2: Authenticate GitHub

```bash
gh auth status
git remote -v
```

Expected: GitHub authentication works and `origin` is the intended repository.

**Purpose:** Prevent pushing or merging in the wrong repository.

### Step 3: Refresh repository state

```bash
git fetch origin --prune
git status --short --branch
```

Expected: No unexpected tracked changes.

**Purpose:** Review and merge against current remote state.

**Stop if:** Tracked local changes are not understood. Preserve untracked review
and planning documents unless they are deliberately committed.

## 2. Use The Common Code Gate

Run this gate on every code phase before approval and again after it reaches
`main`:

```bash
test -z "$(gofmt -l .)"
go build ./...
go vet ./...
go test -race ./...
golangci-lint run
govulncheck ./...
```

Expected: Every command exits zero. `gofmt` and `git diff --check` print nothing.

**Purpose:** Check formatting, compilation, static analysis, races,
vulnerabilities, and whitespace.

For a PR, check committed whitespace with its exact GitHub revisions:

```bash
git diff --check "$base_oid...$head_oid"
```

The PR review procedure below sets both variables.

### Compatibility gate

If a checkout of canonical `landscape-client` is available:

```bash
LANDSCAPE_CLIENT_PATH=/path/to/landscape-client make compat
```

Expected: Python/Go bpickle compatibility tests pass.

**Purpose:** Protect the existing wire format.

**Stop if:** A compatibility test fails. Do not update fixtures just to make it
pass without confirming the protocol change with the server owner.

### Snap gate

Run after phases that change `snap/snapcraft.yaml`, especially Phases 0 and 4:

```bash
snapcraft --destructive-mode
```

Expected: The snap builds successfully with the pinned Go toolchain.

**Purpose:** Go tests do not validate snap packaging.

### Handling a suspected flaky test

1. Run the failing test alone at least 50 times.
2. Run the same command on the phase's unchanged parent.
3. Record both commands and results in the PR.
4. Open a separate issue if it fails on the parent.
5. Stop the merge if it fails only on the phase branch.

Example:

```bash
go test -race ./internal/monitor -run '^TestUsers_CreateUpdateDelete$' -count=50
```

**Purpose:** Separate a regression from existing test instability without hiding
either one.

## 3. Review Each Code Phase

For each phase, first read its detailed plan. Confirm every task is represented by
a commit, test, or documented no-op.

### Phase 0: Foundation

Run:

```bash
git status --short
grep -n 'go-version' .github/workflows/ci.yml .github/workflows/compat.yml
grep -n 'go/1.25' snap/snapcraft.yaml
! git ls-files | grep -Eq '(^|/)(landscape-client-core|landscape-client-core-config)$'
```

Then run the common code gate, compatibility gate, and snap gate.

Check that CI includes race tests, vulnerability scanning, and linting.

**Purpose:** Every later phase depends on trustworthy automation and build inputs.

**Stop if:** Go versions disagree, generated binaries are tracked, or either
compatibility or snap packaging fails.

### Phase 1: P0 defects

Run:

```bash
go test -race ./cmd/... ./internal/bpickle ./internal/exchange ./internal/manager ./internal/monitor
go test ./internal/bpickle -run 'Depth|Unmarshal' -count=20
go test ./internal/manager -run 'TimeLimit|ProcessGroup|Cancel' -count=20
go test ./internal/monitor -run 'NetworkDevice' -count=20
go test ./internal/bpickle -run '^$' -fuzz=FuzzUnmarshal -fuzztime=30s
```

Then run the common code gate and compatibility gate.

**Purpose:** Confirm decoder limits, correct network flags, bounded scripts, and
long-running handler lifetime.

**Stop if:** The fuzzer crashes, child processes survive cancellation, or the
wire-compatibility gate fails.

### Phase 2: P1 reliability

Run:

```bash
go test -race ./internal/transport ./internal/snapd ./internal/manager ./internal/monitor ./internal/persist ./internal/runcmd
go test ./internal/transport -run 'Timeout' -count=20
go test ./internal/persist -run 'Backup|Recover|Save' -count=20
go test ./internal/runcmd -count=20
```

Then run the common code gate.

Check that timeout errors include useful operation context and that failed reads
do not become empty state.

**Purpose:** A disconnected or degraded service must fail safely and remain
diagnosable.

**Stop if:** A blocking operation has no deadline, state recovery loses identity,
or command errors discard stderr context.

### Phase 3: P1 efficiency

Run:

```bash
go test -race ./internal/exchange ./internal/monitor
go test ./internal/exchange -run 'Queue|Backoff|Pending|Interval|Cap' -count=20
go test ./internal/monitor -run 'Ticker|Accumulator|Process|Temperature|Mount' -count=20
```

Then run the common code gate and compatibility gate.

Check that queued messages survive restart, urgent and scheduled messages remain
distinct, and tuple shapes are unchanged.

**Purpose:** Reduce CPU, bandwidth, and allocation without changing server-visible
values.

**Stop if:** Queue tests lose data, retry loops lose backoff, or telemetry tuple
shape changes without an approved protocol decision.

### Phase 4: P2 hygiene

Run:

```bash
! rg -n 'log\.Printf' --glob '*.go' internal cmd
rg -n 'GOMEMLIMIT|GOMAXPROCS|ldflags' snap/snapcraft.yaml
go test -race ./cmd/... ./internal/manager ./internal/monitor ./internal/ping
```

Then run the common code gate and snap gate.

Check that configured log levels control package output and the version has one
source of truth.

**Purpose:** Make diagnostics, lint policy, tests, and snap runtime settings
consistent.

**Stop if:** Legacy logging bypasses `slog`, lint reports issues, or the snap build
cannot inject the version.

### Phase 5: Runtime boundaries

Run:

```bash
git status --short --branch
go test -race ./internal/exchange -run 'TestPerformExchange_(InvokesSubscribersSequentially|HandlerPanicIsContained|HandlerContextUsesDaemonLifetime)' -count=20
go test -race ./internal/snapd -run 'WaitForChange' -count=10
go test -race ./internal/ping -count=20
! rg -n 'time\.After\(' internal/snapd/snapd.go internal/ping/ping.go
rg -n 'clkTck = 100|USER_HZ|CONFIG_HZ' internal/monitor/activeprocessinfo.go
```

Then run the common code gate.

Check that exchange callbacks remain short or hand work to a bounded owner.

**Purpose:** Remove unbounded callback fan-out and timer churn while preserving
operation lifetime and ping interval behavior.

**Stop if:** A callback can be silently dropped, snapd terminal errors poll
forever, or `SetInterval` interrupts an already armed timer.

## 4. Approve A Code PR

For each PR:

1. Fetch and pin its exact base and head:

   ```bash
  pr=4
  base_oid=$(gh pr view "$pr" --json baseRefOid --jq .baseRefOid)
  head_oid=$(gh pr view "$pr" --json headRefOid --jq .headRefOid)
  gh pr view "$pr" --json baseRefName,headRefName,baseRefOid,headRefOid,mergeable,reviewDecision,statusCheckRollup
  git fetch origin "$base_oid" "$head_oid"
  mkdir -p "$HOME/.cache/landscape-client-core"
  printf '%s\n' "$head_oid" > "$HOME/.cache/landscape-client-core/pr-$pr-reviewed-head"
   ```

2. Inspect only that phase's range:

   ```bash
  git diff --stat "$base_oid...$head_oid"
  git log --oneline "$base_oid..$head_oid"
  git diff --check "$base_oid...$head_oid"
   ```

3. Create or reset a disposable review worktree at the exact head:

  ```bash
  review_dir="$HOME/.config/superpowers/worktrees/landscape-client-core/review-pr-$pr"
  test ! -e "$review_dir"
  git worktree add --detach "$review_dir" "$head_oid"
  cd "$review_dir"
  test "$(git rev-parse HEAD)" = "$head_oid"
  ```

4. Run the phase-specific checks in that worktree. The phase blocks are
  checkout-neutral. Do not switch branches or leave this directory.
5. Run the common code gate.
6. Obtain code review approval.
7. Record any accepted residual risk in the PR.
8. Return to the primary checkout and remove the disposable worktree:

  ```bash
  cd "$(git -C "$review_dir" rev-parse --git-common-dir)/.."
  git worktree remove "$review_dir"
  git worktree prune
  ```

**Purpose:** Tests show behavior; review checks scope, ownership, and maintainability.

**Stop if:** The diff contains a later phase, required checks are absent, review
has unresolved important findings, or the PR is not mergeable.

## 5. Merge The Existing Stack

Merge in this exact order:

```text
#4 -> #5 -> #6 -> #7 -> #3 -> #8
```

### Step 1: Merge the current bottom PR

For Phase 0:

```bash
pr=4
reviewed_head_oid=$(cat "$HOME/.cache/landscape-client-core/pr-$pr-reviewed-head")
current_head_oid=$(gh pr view "$pr" --json headRefOid --jq .headRefOid)
test "$current_head_oid" = "$reviewed_head_oid" && \
  gh pr checks "$pr" --watch --fail-fast && \
  test "$(gh pr view "$pr" --json reviewDecision --jq .reviewDecision)" = APPROVED && \
  gh pr merge "$pr" --merge --delete-branch=false --match-head-commit "$reviewed_head_oid"
```

Use `--merge`. Do not use `--squash` or `--rebase`.

**Purpose:** Preserve the commit ancestry used by every child branch.

**Stop if:** Required checks fail, review is not `APPROVED`, or the PR head changes.
Rerun review and tests against the new head instead of removing
`--match-head-commit`.

### Step 2: Update and verify `main`

```bash
main_root=$(git -C "$(git rev-parse --git-common-dir)/.." rev-parse --show-toplevel)
cd "$main_root"
git switch main
git pull --ff-only origin main
go test -race ./...
```

Expected: `main` contains the merged phase and tests pass.

**Purpose:** Verify the result, not only the PR branch.

### Step 3: Retarget the child PR

After merging #4:

```bash
gh pr edit 5 --base main
gh pr view 5 --json baseRefName,headRefName,commits,files
```

Expected: The base is `main` and the PR shows Phase 1 only.

**Purpose:** Turn the next stacked PR into the new bottom PR.

**Stop if:** The child now includes files or commits from an already merged phase.
Do not merge until the diff is understood.

### Step 4: Repeat for every phase

Use this table:

| Merge | Then retarget |
|---|---|
| #4 Phase 0 | `gh pr edit 5 --base main` |
| #5 Phase 1 | `gh pr edit 6 --base main` |
| #6 Phase 2 | `gh pr edit 7 --base main` |
| #7 Phase 3 | `gh pr edit 3 --base main` |
| #3 Phase 4 | `gh pr edit 8 --base main` |
| #8 Phase 5 | No existing child |

For each row:

1. fetch the new PR OIDs and inspect the retargeted diff;
2. wait for CI on the `main`-targeting PR;
3. run its phase checks locally;
4. obtain approval;
5. merge with `--merge`;
6. pull `main` and rerun `go test -race ./...`.

Do not delete the phase branches until #8 has merged.

## 6. Complete Phase 6

Start Phase 6 only after Phase 5 is on `main`. It is a decision phase. It must not
change production behavior.

### Prepare the research inputs

Do this before starting either Phase 6 or Phase 7. Update `main`, then confirm all
required documents exist in the `main` commit tree:

```bash
main_root=$(git -C "$(git rev-parse --git-common-dir)/.." rev-parse --show-toplevel)
cd "$main_root"
git switch main
git pull --ff-only origin main
for path in \
  docs/phase-delivery-runbook.md \
  docs/superpowers/specs/2026-08-28-code-review-follow-up-design.md \
  docs/superpowers/plans/2026-08-28-phase-5-runtime-boundaries.md \
  docs/superpowers/plans/2026-08-28-phase-6-protocol-semantics.md \
  docs/superpowers/plans/2026-08-28-phase-7-device-identity.md
do
  git cat-file -e "main:$path" || exit 1
done
```

If this fails, open and merge a documentation-only PR containing those files
before creating a research worktree. Include the Phase 5 plan too if it is not
already tracked. Do not include `docs/CODE_REVIEW_2026-08.md` unless its author
intends to publish it.

**Purpose:** A new worktree contains committed files only. Untracked local plans
would disappear from the engineer's workflow.

### Step 1: Name the owners and devices

Record owners for:

- client behavior;
- server ingestion and alerts;
- `/ping` and TLS routing;
- representative Ubuntu Core devices.

**Purpose:** Only the system owners can approve wire-visible changes.

**Stop if:** An owner or representative environment is unavailable. Record
`DEFER` for the affected item.

### Step 2: Create the research branch

```bash
go test -race ./...
research_root="$HOME/.config/superpowers/worktrees/landscape-client-core"
mkdir -p "$research_root"
git worktree add "$research_root/research-06-protocol-semantics" \
  -b research/06-protocol-semantics main
cd "$research_root/research-06-protocol-semantics"
```

Create the decision record described in the
[Phase 6 plan](superpowers/plans/2026-08-28-phase-6-protocol-semantics.md).

The commands above replace Task 0 in that plan. Do not create or switch the
research branch a second time.

### Step 3: Gather evidence

Work through the Phase 6 plan in order:

1. test HTTP and HTTPS `/ping` with an approved synthetic identifier;
2. compare legacy free memory with `MemAvailable`;
3. compare `Bfree` with `Bavail`;
4. measure guest CPU double-counting;
5. compare lifetime and interval process CPU.

Use at least the sample counts and environments required by the plan. Commit only
aggregates and conclusions.

**Purpose:** Quantify the effect before changing long-lived protocol semantics.

**Stop if:** A probe lacks permission, a device is not representative, or data
contains identity or credentials.

### Step 4: Trace server consumers

For each metric, identify:

- ingestion code;
- UI labels;
- alert thresholds;
- historical comparisons;
- mixed-client behavior;
- rollback method.

**Purpose:** A locally better formula can still break alerts or historical data.

### Step 5: Select exactly one decision per item

Use only:

- `KEEP`: Current behavior is intentional.
- `CHANGE`: Evidence and owners approve a defined migration.
- `DEFER`: Evidence, ownership, or rollout detail is missing.

Run the decision-record validation commands in the Phase 6 plan.

**Stop if:** Any row has no decision, more than one decision, or unnamed approval.

### Step 6: Plan every approved change

For each `CHANGE`, create a separate implementation plan. Include client and
server changes, compatibility tests, rollout order, observability, and rollback.

Do not implement the change on the research branch.

**Purpose:** Keep evidence review separate from production protocol changes.

### Step 7: Review and merge Phase 6

```bash
git add docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
# Add any approved follow-up plans.
git commit -m "docs: decide wire-visible reporting semantics"
git push -u origin research/06-protocol-semantics
gh pr create --base main --head research/06-protocol-semantics \
  --title "Phase 6: Decide protocol semantics" \
  --body "Records evidence, owner decisions, compatibility impact, and follow-up work."
```

Require client and server-owner review. Merge only the decision and plans.

Review this PR with Section 4. Save its exact reviewed head. Then merge with the
same check, approval, and head-SHA gates used for code PRs:

```bash
pr=$(gh pr view research/06-protocol-semantics --json number --jq .number)
reviewed_head_oid=$(cat "$HOME/.cache/landscape-client-core/pr-$pr-reviewed-head")
current_head_oid=$(gh pr view "$pr" --json headRefOid --jq .headRefOid)
test "$current_head_oid" = "$reviewed_head_oid" && \
  gh pr checks "$pr" --watch --fail-fast && \
  test "$(gh pr view "$pr" --json reviewDecision --jq .reviewDecision)" = APPROVED && \
  gh pr merge "$pr" --merge --delete-branch=false --match-head-commit "$reviewed_head_oid"
```

After merge:

```bash
main_root=$(git -C "$(git rev-parse --git-common-dir)/.." rev-parse --show-toplevel)
cd "$main_root"
git switch main
git pull --ff-only origin main
git log -1 --oneline
```

## 7. Complete Phase 7

Start from `main` after Phase 5. Phase 7 is independent of Phase 6, but doing it
after Phase 6 keeps the delivery sequence simple.

### Step 1: Name the owners and test environment

Required owners:

- Landscape client;
- Landscape server authentication;
- snapd device identity;
- snap confinement/security;
- brand/account key operations.

Required environments:

- isolated staging server;
- disposable Ubuntu Core device with serial and model assertions.

**Stop if:** The server or snapd owner is unavailable. The correct result is
`BLOCKED`, not an inferred design.

### Step 2: Create the research branch

Repeat **Prepare the research inputs** above if Phase 7 starts independently.
Then run:

```bash
git switch main
git pull --ff-only origin main
go test -race ./internal/snapd ./internal/exchange ./integration
research_root="$HOME/.config/superpowers/worktrees/landscape-client-core"
mkdir -p "$research_root"
git worktree add "$research_root/research-07-device-identity" \
  -b research/07-device-identity main
cd "$research_root/research-07-device-identity"
```

Create the decision record described in the
[Phase 7 plan](superpowers/plans/2026-08-28-phase-7-device-identity.md).

The commands above replace Task 0 in that plan. Do not create or switch the
research branch a second time.

### Step 3: Document the current trust flow

Record:

- assertion retrieval and fields retained by the client;
- shared-secret registration;
- server-issued bearer credentials;
- identity-cloning risk;
- absence of a device-key signature over a fresh challenge.

**Purpose:** State the current security boundary before proposing a stronger one.

### Step 4: Inspect assertions safely

On the disposable device, capture serial and model assertions only outside the
repository. Record redacted structure, versions, and hashes. Delete raw captures
when complete.

Never commit a real assertion, serial, device key, account key, registration
secret, computer ID, exchange token, nonce, or credential-bearing response.

**Purpose:** Verify the real format without creating a credential leak.

### Step 5: Verify snapd capabilities

Pin the device's snapd source revision. Confirm:

- how the model authorizes a serial authority;
- how the account/account-key chain is verified;
- where the device private key lives;
- whether a confined API can sign a fresh challenge;
- whether a new interface would be required.

Obtain confirmation from the snapd and security owners. A source-code search alone
is not enough.

### Step 6: Define server behavior

Specify trust-chain verification, key rotation, revocation, replay protection,
challenge encoding, nonce lifetime, mixed fleets, downgrade rules, bearer-token
behavior, rollout signals, and recovery.

**Purpose:** Assertion forwarding alone is attestation. It is not proof of current
possession of the device key.

### Step 7: Select exactly one outcome

Use one:

- `APPROVE_PROOF_OF_POSSESSION`;
- `APPROVE_ATTESTATION`;
- `TELEMETRY_ONLY`;
- `REJECT`;
- `BLOCKED`.

Run every validation and secret-review command in the Phase 7 plan.

**Stop if:** Authentication correctness depends on an unanswered question. Select
`BLOCKED` and name the owner and reopening condition.

### Step 8: Create only approved follow-up work

Use the selected outcome:

| Outcome | Follow-up |
|---|---|
| `APPROVE_PROOF_OF_POSSESSION` | Create client, server, and snapd implementation plans. |
| `APPROVE_ATTESTATION` | Create client and server plans. Do not claim possession proof. |
| `TELEMETRY_ONLY` | Plan provenance telemetry only. Keep shared-secret authorization. |
| `REJECT` | Create no implementation plan. Record the reason. |
| `BLOCKED` | Create owner-assigned investigation issues and reopening conditions only. |

Do not add dormant fields, speculative crypto dependencies, or dead feature
flags.

### Step 9: Review and merge Phase 7

```bash
git add docs/superpowers/specs/2026-08-28-device-identity-decision.md
# Add a client plan only when the selected outcome requires one.
git commit -m "docs: decide serial-assertion onboarding"
git push -u origin research/07-device-identity
gh pr create --base main --head research/07-device-identity \
  --title "Phase 7: Decide device identity design" \
  --body "Records assertion evidence, trust boundaries, owner decisions, and follow-up work."
```

For an approval outcome, require dated approval from client, server, snapd, and
security owners. For `REJECT`, require the deciding product/security owners. For
`BLOCKED`, require the available owners to confirm the blocker, assignee, and
reopening condition. Merge only when the outcome and residual risks are explicit.

Review this PR with Section 4. Save its exact reviewed head. Then run:

```bash
pr=$(gh pr view research/07-device-identity --json number --jq .number)
reviewed_head_oid=$(cat "$HOME/.cache/landscape-client-core/pr-$pr-reviewed-head")
current_head_oid=$(gh pr view "$pr" --json headRefOid --jq .headRefOid)
test "$current_head_oid" = "$reviewed_head_oid" && \
  gh pr checks "$pr" --watch --fail-fast && \
  test "$(gh pr view "$pr" --json reviewDecision --jq .reviewDecision)" = APPROVED && \
  gh pr merge "$pr" --merge --delete-branch=false --match-head-commit "$reviewed_head_oid"
```

After merge:

```bash
main_root=$(git -C "$(git rev-parse --git-common-dir)/.." rev-parse --show-toplevel)
cd "$main_root"
git switch main
git pull --ff-only origin main
git log -1 --oneline
```

## 8. Deliver Conditional Implementation Work

A Phase 6 `CHANGE` or Phase 7 approval creates new implementation work. It does
not mean the change is already delivered.

For each approved plan:

1. Create a branch from current `main`.
2. Add a failing compatibility or security test first.
3. Implement the smallest approved change.
4. Run focused tests.
5. Run the common code gate.
6. Test old-client/new-server and new-client/old-server behavior.
7. Deploy server support before clients when the plan requires it.
8. Verify rollout metrics in staging.
9. Open a PR against `main`.
10. Merge only after rollback has been exercised or demonstrated.

**Purpose:** Research approval defines a safe change. It does not bypass normal
implementation and release controls.

## 9. Final Verification

After all approved work is merged:

```bash
git switch main
git pull --ff-only origin main
test -z "$(gofmt -l .)"
go build ./...
go vet ./...
go test -race ./...
golangci-lint run
govulncheck ./...
git status --short --branch
```

Also run the compatibility and snap gates when their prerequisites are available.

Confirm repository state:

```bash
gh pr list --state open
git branch --merged main
git worktree list
```

Expected:

- no phase PR that should be merged remains open;
- `main` passes all checks;
- only deliberately deferred or blocked work remains;
- the primary checkout has no unexpected tracked changes.

## 10. Clean Up

Only after all stacked PRs are merged and no child PR uses their bases:

```bash
main_root=$(git -C "$(git rev-parse --git-common-dir)/.." rev-parse --show-toplevel)
cd "$main_root"
for completed_branch in \
  fix/00-foundation \
  fix/01-p0-defects \
  fix/02-p1-reliability \
  fix/03-p1-efficiency \
  fix/04-p2-hygiene \
  fix/05-runtime-boundaries \
  research/06-protocol-semantics \
  research/07-device-identity
do
  git ls-remote --exit-code --heads origin "$completed_branch" >/dev/null || continue
  git push origin --delete "$completed_branch"
done
```

List every owned worktree and remove only completed ones:

```bash
git worktree list
worktree_root="$HOME/.config/superpowers/worktrees/landscape-client-core"
for worktree_path in \
  "$worktree_root/fix-05-runtime-boundaries" \
  "$worktree_root/research-06-protocol-semantics" \
  "$worktree_root/research-07-device-identity"
do
  test ! -d "$worktree_path" || git worktree remove "$worktree_path"
done
git worktree prune
for completed_branch in \
  fix/00-foundation \
  fix/01-p0-defects \
  fix/02-p1-reliability \
  fix/03-p1-efficiency \
  fix/04-p2-hygiene \
  fix/05-runtime-boundaries \
  research/06-protocol-semantics \
  research/07-device-identity
do
  git show-ref --verify --quiet "refs/heads/$completed_branch" || continue
  git branch -d "$completed_branch"
done
```

If another path appears in `git worktree list`, determine who owns it before
removal. Do not remove an active worktree or a branch used by an open PR.

**Purpose:** Cleanup is last because stacked PRs depend on branch ancestry and
engineers may need worktrees for review fixes.

## Completion Checklist

- [ ] Phases 0-5 merged in order with merge commits.
- [ ] Each child PR was retargeted to `main` and its diff rechecked.
- [ ] Common code gate passed after every merge.
- [ ] Compatibility and snap gates passed where applicable.
- [ ] Phase 6 has five explicit, owner-reviewed decisions.
- [ ] Every Phase 6 `CHANGE` has a separate delivery plan.
- [ ] Phase 7 has one explicit, owner-reviewed outcome.
- [ ] Phase 7 follow-up matches its outcome matrix; no unapproved component work exists.
- [ ] No secret or device identity material is committed.
- [ ] Conditional implementation work is merged or explicitly tracked.
- [ ] Final `main` verification passed.
- [ ] Obsolete branches and owned worktrees were removed last.
