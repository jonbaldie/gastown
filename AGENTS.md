# Agent Instructions

**jonbaldie/gastown** is home. Ship to `origin`. `upstream` is `gastownhall/gastown` for syncs when asked. For a `gastownhall/gastown` contribution, read CONTRIBUTING.md.

> **Recovery**: Run `gt prime` after compaction, clear, or new session

Domain language: [`CONTEXT.md`](CONTEXT.md) — read it when naming Town, Rig, Role, Bead, Hook, or other glossary terms.

Full context is injected by `gt prime` at session start.

<!-- beads-agent-instructions-v2 -->

---

## Beads

Use `bd` to track planned work and genuine follow-up. Do not create a Bead
solely to ship, review, or merge an existing pull request.

```bash
bd ready              # Unblocked Beads
bd show <id>          # Details and dependencies
bd create --title="..." --type=task --priority=2
bd update <id> --claim
bd close <id>
```

Use `bv` only with `--robot-*` flags; bare `bv` opens an interactive UI.

If this Session changed Bead state, run `bd dolt push` once after the code is
pushed. If it fails, record the exact error in the handoff; do not start
parallel pushes, repeatedly retry, or change transport settings automatically.
Beads replication does not block a verified code delivery.

To finish a Session: run proportionate quality checks, commit and push the
code, then clean up temporary worktrees. File Beads only for work that remains.

<!-- end-beads-agent-instructions -->

<!-- gastown-agent-instructions-v1 -->

---

## Gas Town Multi-Agent Communication

This workspace is part of a **Gas Town** multi-agent environment. You communicate
with other agents using `gt` commands — never by printing text or using raw tmux.

### Nudging Agents (Immediate Delivery)

`gt nudge` sends a message directly to another agent's active session:

```bash
gt nudge mayor "Status update: PR review complete"
gt nudge laneassist/crew/dom "Check your mail — PR ready for review"
gt nudge witness "Polecat health check needed"
gt nudge refinery "Merge queue has items"
```

**Target formats:**
- Role shortcuts: `mayor`, `deacon`, `witness`, `refinery`
- Full path: `<rig>/crew/<name>`, `<rig>/polecats/<name>`

**Important:** `gt nudge` is the ONLY way to send text to another agent's session.
Never print "Hey @name" — the other agent cannot see your terminal output.

### Sending Mail (Persistent Messages)

`gt mail` sends messages that persist across session restarts:

```bash
# Reading
gt mail inbox                    # List messages
gt mail read <id>                # Read a specific message

# Sending (use --stdin for multi-line content)
gt mail send mayor/ -s "Subject" -m "Short message"
gt mail send laneassist/crew/dom -s "PR Review" --stdin <<'BODY'
Multi-line message content here.
Details about the PR and what to look for.
BODY
gt mail send --human -s "Subject" -m "Message to overseer"
```

### When to Use Which

| Want to... | Command | Why |
|------------|---------|-----|
| Wake a sleeping agent | `gt nudge <target> "msg"` | Immediate delivery |
| Send detailed task/info | `gt mail send <target> -s "..." --stdin` | Persists across restarts |
| Both: send + wake | `gt mail send` then `gt nudge` | Mail carries payload, nudge wakes |

### Context Recovery

After compaction or new session, run `gt prime` to reload your full role context,
identity, and any pending work.

```bash
gt prime              # Full context reload
gt hook               # Check for assigned work
gt mail inbox         # Check for messages
```

<!-- end-gastown-agent-instructions -->

---

## Cursor Cloud specific instructions

Gas Town is a Go CLI (`gt`). The environment build already has: Go (base `go1.22.2`, but
`go.mod` pins `go 1.26.5`, auto-fetched via `GOTOOLCHAIN=auto`), `libicu-dev` (required for the
`go-icu-regex` CGo dependency), `golangci-lint` v2.11.4, `bd` (beads), and `dolt` v2.0.7. The
update script runs `go mod download` to refresh module deps after a pull.

### Build / lint / test / run

- Build: `make build` → produces `gt`, `gt-proxy-server`, `gt-proxy-client` at repo root.
  Default is `CGO_ENABLED=0` (no beads embedded Dolt engine).
  `make build-dev` builds only `gt`. `make build-cgo` restores the CGO graph.
  Bare `go build` / `go run` do not set `CGO_ENABLED`; use `make` or `CGO_ENABLED=0`.
- Lint: `golangci-lint run --timeout=5m` (installed at `~/go/bin`).
  - GOTCHA: `golangci-lint` refuses to run if it was built with an older Go than `go.mod`'s
    `1.26.5` ("the Go language version go1.25 ... is lower than the targeted Go version 1.26.5").
    `go install ...@v2.11.4` picks the tool's own toolchain (1.25). It must be built with
    `GOTOOLCHAIN=go1.26.5 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`.
    The prebuilt binary in the snapshot is already correct.
- Test (unit): `go test -short ./...`. Full suite is slow (~15-20 min) because many packages spawn
  real `git`/`tmux` subprocesses.
- Integration tests are build-tagged and need Dolt: `go test -tags=integration ./internal/cmd/...`.
  Dolt helpers in `internal/testutil` compile only with `-tags=integration` and prefer a native `dolt sql-server`.
  Default `go test` uses skip stubs so unit compiles do not pull testcontainers.
- Run the app: from a project git repository, `gt now` starts a town and attaches to the Mayor.
  `gt install <path>` still creates a dedicated HQ. Then `gt status`, `bd create/list/update/close`,
  `gt rig add`. `gt` must be on `PATH` (`cp gt ~/go/bin/` or `make install` → `~/.local/bin`).

### Known environment-induced test failures (NOT code bugs)

Under a full `go test ./...` in the cloud VM, these fail purely due to the VM's global git config
and full-suite parallelism — they all pass when isolated:

- `internal/git` (`TestConfigurePushURL`, `TestGetPushURL_NoPushURL`, `TestClearPushURL`) and
  `internal/doctor` (`TestBareRepoExistsCheck_*PushURL*`): the cloud VM injects Cursor-managed
  `url.https://x-access-token:***@github.com/.insteadOf` rewrites into the global git config, so
  round-tripped remote URLs don't match. Re-run with a clean config to confirm green, e.g.
  `TMP=$(mktemp); printf '[user]\n\tname=t\n\temail=t@t' >$TMP; GIT_CONFIG_GLOBAL=$TMP go test ./internal/git/ ./internal/doctor/`.
- `internal/config` `TestBuiltinPresets` / `TestRuntimeConfigFromPreset` can flake under
  `./...` when a sibling test overlays the live agent registry (`Command = env`,
  empty `ProcessNames`). Those tests now read the compile-time `builtinPresets`
  table. They pass reliably via `go test ./internal/config/`.

### Agent skills

Project skills live in `.agents/skills/`. That tree vendors
[`mattpocock/skills`](https://github.com/mattpocock/skills) and
[`jonbaldie/skills`](https://github.com/jonbaldie/skills) so `/ship-spec` can
reach `/implement` in the same directory. See `.agents/skills/NOTICE` for the
pinned commits and license.

Refresh from the collection installer:

```bash
curl -fsSL https://raw.githubusercontent.com/jonbaldie/skills/main/install.sh | bash -s -- --project --agent universal --copy --with-prereqs --yes
```

The environment snapshot may still install the same collections globally under
`~/.agents/skills/` and mirror them to `~/.cursor/skills-cursor/` (Cursor Cloud
discovers global skills there, not `~/.cursor/skills`). That snapshot copy is a
fallback for sessions that boot before a pull. The in-repo tree is the source of
truth for this project. The snapshot refresh is best-effort (`|| true`). Changes
under `.agents/` skip the Go CI workflows.
