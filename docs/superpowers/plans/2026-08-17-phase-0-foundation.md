# Phase 0 — Foundation Implementation Plan

> **For agentic workers:** REQUIRED: Use the `subagent-driven-development` agent (recommended) or `executing-plans` agent to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make CI a meaningful gate for every later phase, and stop build artifacts accumulating in git.

**Architecture:** No production code changes. Three commits: finish the Go 1.25 alignment that PR #2 started but left incomplete in the compat workflow and the snap build; add `-race` and `govulncheck` to CI; add the missing `.gitignore` and untrack 15 committed build artifacts.

**Tech Stack:** GitHub Actions, snapcraft, git

**Spec:** [docs/superpowers/specs/2026-08-17-code-review-remediation-design.md](../specs/2026-08-17-code-review-remediation-design.md)

**Branch:** `fix/00-foundation`, cut from `main` at `a1cfeae`

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `.github/workflows/compat.yml` | Modify | Go `1.22` → `1.25` |
| `snap/snapcraft.yaml` | Modify | `go/1.22/stable` → `go/1.25/stable` |
| `.github/workflows/ci.yml` | Modify | `-race` on the test step; add a `govulncheck` step |
| `.gitignore` | Create | Does not currently exist |

---

## Task 0: Create the branch

- [ ] **Step 1: Cut the branch from main**

```bash
git checkout main
git pull
git checkout -b fix/00-foundation
```

- [ ] **Step 2: Record the baseline**

Run: `git rev-parse --short HEAD`
Expected: `a1cfeae` (or later, if `main` has moved — note the actual value)

---

## Task 1: Align Go 1.25 in the compat workflow and snap build

`go.mod` declares `go 1.25.0`. PR #2 fixed `ci.yml` but left two other pins at
1.22. A 1.22 toolchain cannot build a `go 1.25.0` module, so the bpickle
wire-compatibility job — the single best test asset in the repo — and the snap
build are both on a broken pin.

`go/1.25/stable` exists in the Go snap (1.25.12, revision 11218), so this is a
one-line change in each file.

**Files:**
- Modify: `.github/workflows/compat.yml:26`
- Modify: `snap/snapcraft.yaml:69`

- [ ] **Step 1: Confirm the mismatch exists**

Run:

```bash
head -3 go.mod
grep -n 'go-version' .github/workflows/compat.yml
grep -n 'go/1' snap/snapcraft.yaml
```

Expected output:

```
module github.com/canonical/landscape-client-core

go 1.25.0
26:          go-version: "1.22"
69:      - go/1.22/stable
```

If either already reads `1.25`, that half of this task is done — skip it and note
so in the commit message.

- [ ] **Step 2: Update the compat workflow**

In `.github/workflows/compat.yml`, change:

```yaml
      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: "1.22"
          cache: true
```

to:

```yaml
      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true
```

- [ ] **Step 3: Update the snap build**

In `snap/snapcraft.yaml`, change:

```yaml
    build-snaps:
      - go/1.22/stable
```

to:

```yaml
    build-snaps:
      - go/1.25/stable
```

- [ ] **Step 4: Verify no 1.22 pins remain**

Run: `grep -rn '1\.22' .github/workflows/ snap/snapcraft.yaml go.mod`
Expected: no output.

- [ ] **Step 5: Verify the module still builds locally**

Run: `go build ./... && go vet ./...`
Expected: no output, exit 0.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/compat.yml snap/snapcraft.yaml
git commit -m "ci: align Go 1.25 in compat workflow and snap build

go.mod requires go 1.25.0, but the compat workflow and the snapcraft
build-snaps entry were still pinned to 1.22, which cannot build the module.
Completes the alignment started in 0a0234e, which only covered ci.yml."
```

---

## Task 2: Run tests under `-race` and add `govulncheck`

The suite is currently race-clean. Nothing enforces that. For a network-facing
daemon, a vulnerability scan is also cheap to add.

**Files:**
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Confirm the current test step and absence of govulncheck**

Run:

```bash
grep -n 'go test' .github/workflows/ci.yml
grep -rn 'race\|govulncheck' .github/workflows/ || echo NONE
```

Expected:

```
        run: go test ./...
NONE
```

- [ ] **Step 2: Verify the suite is actually race-clean before enforcing it**

Run: `go test -race ./...`
Expected: all packages `ok` or `no test files`. If anything fails, stop — fixing
a real race is not part of this task and must be raised before proceeding, since
enabling the gate would block every later phase.

- [ ] **Step 3: Add `-race` to the test step**

In `.github/workflows/ci.yml`, change:

```yaml
      - name: Test
        run: go test ./...
```

to:

```yaml
      - name: Test
        run: go test -race ./...
```

- [ ] **Step 4: Add the govulncheck step**

In `.github/workflows/ci.yml`, insert after the `Test` step and before the `Lint`
step:

```yaml
      - name: Vulnerability scan
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck ./...
```

- [ ] **Step 5: Run govulncheck locally to confirm it passes**

Run:

```bash
go install golang.org/x/vuln/cmd/govulncheck@latest
"$(go env GOPATH)"/bin/govulncheck ./...
```

Expected: `No vulnerabilities found.`

If vulnerabilities *are* found, do not silence the step. Record the findings in
the commit message, bump the affected dependency in a separate commit within this
task, and re-run. The dependency tree is 4 modules, all Go-team owned except
`github.com/godbus/dbus/v5`, so an upgrade is low risk.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: run tests under -race and add govulncheck

The suite is race-clean today; this locks that in. govulncheck is cheap
insurance for a network-facing daemon that parses untrusted server input."
```

---

## Task 3: Add `.gitignore` and untrack build artifacts

There is no `.gitignore` at all, so every build artifact is offered to `git add`
on every commit. `.git` is 226 MiB, dominated by 8 committed `.snap` packages
(54 MB). A `.snap` is a squashfs image — incompressible to git, and each rebuild
stores a brand-new full blob.

Note the `-dirty` suffixes and `good-working-version` among them: those are
uncommitted-tree snapshots, i.e. exactly the local scratch state version control
is meant to replace. They also carry a supply-chain smell, since a prebuilt binary
in-tree can be consumed by someone assuming it matches the source beside it, and a
`-dirty` snap by definition does not.

**This task does not reclaim the 224 MiB** — the blobs stay reachable from history.
That requires `git filter-repo`, which rewrites every hash and is deliberately out
of scope (see spec §6).

**Files:**
- Create: `.gitignore`

- [ ] **Step 1: List what is currently tracked**

Run:

```bash
git ls-files | grep -E '\.snap$|^landscape-client-core$|^landscape-client-core-config$|\.patch$|test_output|test_results|fix_|snapcraft_output'
```

Expected (15 paths):

```
fix_mountinfo.patch
fix_transport.py
internal/transport/transport.patch
landscape-client-core
landscape-client-core_0+git.75aec9b-dirty_amd64.snap
landscape-client-core_0+git.75aec9b_amd64.snap
landscape-client-core_0+git.7f5508a_amd64.snap
landscape-client-core_0+git.e2a0966-dirty_amd64.snap
landscape-client-core_0+git.e2a0966_amd64.snap
landscape-client-core_0+git.ea82cf8_amd64.snap
landscape-client-core_26.04_amd64.snap
landscape-client-core_good-working-version-dirty_amd64.snap
test_output.log
test_output.txt
test_results.txt
```

`landscape-client-core-config` may or may not appear depending on whether it was
committed; handle whatever the command actually returns.

- [ ] **Step 2: Read the scratch files before deleting anything from the index**

Run:

```bash
head -40 fix_transport.py
head -40 fix_mountinfo.patch
head -40 internal/transport/transport.patch
```

These are ad-hoc patches. Confirm their content is already reflected in the
working tree (it should be — `git status` is clean and `gofmt -l .` passes). If any
contains an unapplied change, raise it rather than untracking it silently; an
unapplied fix is in-progress work, not scratch.

- [ ] **Step 3: Create `.gitignore`**

Create `.gitignore` with exactly this content:

```gitignore
# Snap build output — release artifacts belong in the Snap Store, not git
*.snap
parts/
prime/
stage/
snapcraft_output.log

# Compiled binaries (built by `go build ./cmd/...`)
/landscape-client-core
/landscape-client-core-config

# Test and scratch output
*.log
test_output.txt
test_results.txt
*.patch
```

The leading `/` on the two binary entries is deliberate: it anchors them to the
repo root so they never match a same-named directory or package path.

- [ ] **Step 4: Untrack the artifacts, keeping the local files**

```bash
git rm --cached -- '*.snap' landscape-client-core snapcraft_output.log \
  test_output.log test_output.txt test_results.txt \
  fix_mountinfo.patch fix_transport.py internal/transport/transport.patch
```

If `git rm --cached` reports a path as not in the index, drop it from the command
and continue — the Step 1 output is authoritative. If
`landscape-client-core-config` appeared in Step 1, add it to the command.

- [ ] **Step 5: Verify the artifacts are untracked but still on disk**

Run:

```bash
git ls-files | grep -cE '\.snap$' || echo "0 snaps tracked"
ls -1 *.snap | wc -l
git status --short | head -20
```

Expected: `0 snaps tracked`, a non-zero count of `.snap` files still present on
disk, and a `git status` showing only staged deletions plus the new `.gitignore`.

- [ ] **Step 6: Verify the ignore rules actually work**

Run:

```bash
git check-ignore -v landscape-client-core_26.04_amd64.snap test_output.txt landscape-client-core
```

Expected: three lines, each naming `.gitignore` and the matching pattern.

- [ ] **Step 7: Commit**

```bash
git add .gitignore
git commit -m "chore: add .gitignore and untrack build artifacts

No .gitignore existed, so 8 .snap packages (54 MB), the unstripped binary
and assorted log/patch scratch files were committed. Untracking keeps the
local files but stops further blobs accumulating.

Note this does not shrink the 224 MiB pack — the blobs remain reachable
from history. Reclaiming that needs git filter-repo, which rewrites every
hash and must be coordinated separately."
```

---

## Task 4: Verify the phase

- [ ] **Step 1: Full local verification**

Run:

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

Expected: `gofmt -l .` prints nothing; `go vet` prints nothing; all test packages
report `ok` or `no test files`.

- [ ] **Step 2: Lint with the pinned version**

Run: `golangci-lint run`
Expected: no findings. If `golangci-lint` is not installed locally, note that CI
covers it and proceed.

- [ ] **Step 3: Confirm the working tree is clean**

Run: `git status --short`
Expected: no output.

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin fix/00-foundation
```

PR title: `Phase 0: CI gating and repository hygiene`

PR body should state that CI was not previously a meaningful gate (compat
workflow and snap build pinned to Go 1.22 against a `go 1.25.0` module), and that
the 224 MiB pack is untouched by design.

---

## Done when

- No `1.22` pin remains in `.github/workflows/` or `snap/snapcraft.yaml`.
- CI runs `go test -race ./...` and `govulncheck ./...`.
- `.gitignore` exists and `git ls-files` returns no `.snap`, binary, log or patch artifacts.
- `git status` is clean and all local artifact files still exist on disk.
