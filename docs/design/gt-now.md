# Spec: `gt now` — five-second Mayor session

Triage: `ready-for-agent`

This spec uses the terms in `docs/glossary.md`. This repo has no `CONTEXT.md`.

**Test seam:** the `gt now` command. Tests call this command as a user does. Tests check only what the user can observe.

## Problem Statement

I want to stand in a git repository and start a working Town. I want one command. I want to sit in the Mayor session in five seconds.

I want one model and effort for the Mayor. I want another model and effort for the Witness, Polecats, and other workers.

Today I cannot do that.

A Town is the headquarters. A Rig is the project under that Town. My git repository is not a Town. I must run `gt install`, then `gt rig add`, then `gt config mix`, then `gt up`, then `gt mayor attach`.

A timed run on this tree showed:

- `gt install` takes about 7.6 seconds. Beads and Dolt use most of that time.
- `gt install` into my git repository turns the repository into a Town. The next step is still “add a Rig”.
- `gt rig add` clones the repository again. A one-file repo took 2.0 seconds. This gastown repo took 13.6 seconds.
- `gt config mix` is fast (about 20 milliseconds). It has no `model=` field. I must first create an agent alias whose command holds `--model`.
- `gt config cost-tier` overwrites a custom mix with Claude Opus, Sonnet, and Haiku.
- Effort is not the same for every runtime. Pi gets `--thinking`. Claude gets an environment variable. Cursor stores effort but spawn does not pass a Cursor effort flag.
- `gt up` starts tmux sessions. It failed here because `cursor-agent` and `claude` were not on the machine. `gt doctor` still passed.
- The Cursor preset waits 5 seconds before Gas Town treats the pane as ready. That wait uses the full five-second limit.
- A stale `gt` on `PATH` did not have `gt config mix`. This source tree does.

I cannot say: this git repository, Mayor model A, workers model B with low effort, go.

## Solution

Add one command: `gt now`.

I run `gt now` in a git repository. The command:

1. Checks that an agent CLI is on `PATH`. If none is present, it stops. It does not change the disk.
2. Finds or creates a Town. The Town is not my git repository. The default Town is `~/gt`, or `$GT_TOWN_ROOT` if that is set.
3. Registers this git repository as a Rig. It does not copy the repository over the network. It does not write Mayor, Deacon, or Beads files into my git repository.
4. Sets the mix. `--mayor` sets the Mayor (and the Deacon). `--workers` sets the Witness, Polecats, Refinery, Crew, Boot, and Dogs.
5. Starts Dolt (or reuses it), the daemon, and the Mayor session. It does not wait for the Witness or the Refinery before I attach.
6. Attaches this terminal to the Mayor session.

The clock starts when I run `gt now`. The clock stops when I am in the Mayor session, or when the Mayor session is running if I passed `--no-attach`.

The limit is five seconds for a local git repository when the agent CLI is already on `PATH`.

A second `gt now` in the same repository is a no-op plus attach. It also stays inside five seconds.

## User Stories

1. As an Overseer, I want to run `gt now` in a git repository, so that I enter a working Mayor session without a setup list.
2. As an Overseer, I want that path to finish in five seconds, so that Town start feels instant.
3. As an Overseer, I want one command, so that I do not learn `install`, `rig add`, `mix`, `up`, and `attach`.
4. As an Overseer, I want `gt now` with no flags, so that a good default mix is enough to start.
5. As an Overseer, I want `gt now` to pick a runtime that is already on `PATH`, so that I do not name Claude when I use Cursor.
6. As an Overseer, I want the default Mayor effort to be high, so that the Mayor thinks hard.
7. As an Overseer, I want the default worker effort to be low, so that Patrol and Polecats stay cheap.
8. As an Overseer, I want `--mayor cursor:high`, so that I set Mayor runtime and effort in one flag.
9. As an Overseer, I want `--mayor cursor:grok-4.6:high`, so that I set Mayor runtime, model, and effort in one flag.
10. As an Overseer, I want `--workers cursor:grok-4.6:low`, so that Witness, Polecats, and other workers share one cheap profile.
11. As an Overseer, I want Pi models that contain `/`, so that `--mayor pi:openai-codex/gpt-5.6-luna:high` works.
12. As an Overseer, I want `gt now` to create the agent aliases for me, so that I do not run `gt config agent set`.
13. As an Overseer, I want the mix to apply before the Mayor session starts, so that the Mayor process uses the new model.
14. As an Overseer, I want `gt now` to leave my git repository as a git repository, so that install does not add `mayor/`, `deacon/`, or `AGENTS.md` to my project.
15. As an Overseer, I want the Town to live in `~/gt` by default, so that many projects share one Town.
16. As an Overseer, I want `$GT_TOWN_ROOT` to select the Town, so that I can keep a second Town.
17. As an Overseer, I want `--town` to select the Town for this run, so that a test Town does not use `~/gt`.
18. As an Overseer, I want `gt now` to reuse an existing Town, so that a second project starts faster.
19. As an Overseer, I want `gt now` to pick a free Dolt port when the default port is in use, so that a second Town does not fail.
20. As an Overseer, I want `gt now` to print the Dolt port it chose, so that I can find the Town later.
21. As an Overseer, I want this git repository registered as a Rig, so that Polecats can later work on it.
22. As an Overseer, I want Rig registration without a network clone, so that a large local repo still fits in five seconds.
23. As an Overseer, I want the Rig name to come from the directory name, so that I do not invent a name.
24. As an Overseer, I want `--name` to set the Rig name, so that two folders with the same name do not clash.
25. As an Overseer, I want `gt now` to detect that this repository is already a Rig, so that it does not register it twice.
26. As an Overseer, I want Polecat worktrees to come from this repository, so that workers edit the same project I opened.
27. As an Overseer, I want `gt now` to start the Mayor session, so that I have a Chief of Staff immediately.
28. As an Overseer, I want `gt now` to start the Deacon, so that Patrol can begin after I attach.
29. As an Overseer, I want `gt now` to attach my terminal to the Mayor session, so that I am in the room, not looking at a “next steps” list.
30. As an Overseer, I want `--no-attach` for scripts, so that automation can start a Town without a tmux attach.
31. As an Overseer, I want `gt now` to skip Witness and Refinery start before attach, so that those sessions do not consume the five-second limit.
32. As an Overseer, I want Witness and Refinery to start later as they do with `gt start` today, so that the floor still comes up.
33. As an Overseer, I want `gt now` to fail before it writes files when no agent CLI is on `PATH`, so that I get one clear error.
34. As an Overseer, I want that error to name the missing binary, so that I know what to install.
35. As an Overseer, I want `gt now` to fail before it writes files when the current directory is not a git repository, so that I do not create a Town around a random folder.
36. As an Overseer, I want `gt now` to fail if I run it inside an existing Town HQ by mistake when I meant a project, unless the current directory is already a registered Rig.
37. As an Overseer, I want a second `gt now` to attach to the running Mayor, so that restart is the same command as start.
38. As an Overseer, I want a second `gt now` with new `--mayor` flags to restart only the Mayor session, so that a model change does not rebuild the Town.
39. As an Overseer, I want a second `gt now` with new `--workers` flags to save the mix for the next Witness and Polecat spawn, so that running Patrol is not killed unless I ask.
40. As an Overseer, I want `gt now --restart-workers` when I do need running Witness and Polecat sessions to pick up the new mix.
41. As an Overseer, I want `gt now` not to wait for the Cursor 5-second ready delay before attach, so that attach itself is the ready state.
42. As an Overseer, I want Beads and Dolt to be ready enough for `gt hook` and `gt mail inbox` in the Mayor session, so that GUPP still holds.
43. As an Overseer, I want heavy install work (formula copy, skill trees, slash commands) to finish after attach when it cannot fit in five seconds, so that I am in the Mayor session first.
44. As an Overseer, I want that deferred work to be safe if I detach at once, so that a short visit still leaves a complete Town.
45. As an Overseer, I want `gt now` to be idempotent, so that a crash in the middle can be retried with the same command.
46. As an Overseer, I want `gt config mix` to show the mix that `gt now` wrote, so that the old command still tells the truth.
47. As an Overseer, I want `gt config cost-tier` to warn that it will overwrite a `gt now` mix, as it does for a custom mix today.
48. As an Overseer, I want `gt now` not to call `cost-tier`, so that a Claude Opus/Sonnet/Haiku preset does not replace my Cursor mix.
49. As an Overseer, I want effort to reach the runtime that I chose, so that Cursor, Claude, and Pi each get a native effort control when they have one.
50. As an Overseer, I want `gt now` to reject an effort that the runtime does not support, so that I do not store a silent no-op.
51. As an Overseer, I want `gt now` to reject an unknown runtime name, so that a typo does not create a broken alias.
52. As an Overseer, I want `gt now` to check that the runtime binary is on `PATH`, so that mix does not record a missing command.
53. As an Overseer, I want `gt status` after `gt now` to show the Mayor as running with the chosen runtime and model.
54. As an Overseer, I want `gt doctor` after `gt now` to fail if the Mayor binary is missing, so that doctor matches `gt now`.
55. As an Overseer, I want `gt now --help` to show the five-second path in a few lines, so that I do not open INSTALLING.md to start.
56. As an Overseer, I want `gt now` to print one line with Town path, Rig name, mix, and Dolt port, so that I know where I am before attach.
57. As an Overseer, I want `gt now` to work when I pass a path argument, so that I can start a Town for a repo I am not inside.
58. As an Overseer, I want `gt now` on a repo with no commits to fail with a clear error, as `gt rig add` does today.
59. As an Overseer, I want `gt now` not to create a GitHub remote, so that start does not need network auth.
60. As an Overseer, I want `gt now` not to run `gt enable` or install shell hooks, so that a throwaway Town does not change my machine.
61. As an Overseer, I want `gt now` not to require `gt install --shell`, so that a second Town does not enable Gas Town host-wide.
62. As a Crew member, I want `gt now` to leave room for `gt crew add` later, so that my human workspace is still a later step.
63. As a Polecat, I want the mix from `gt now --workers` when I spawn, so that I use the cheap model the Overseer chose.
64. As a Witness, I want the same worker mix, so that Patrol is cheap.
65. As a Deacon, I want the Mayor profile, so that the Town office shares one model.
66. As a Boot Dog, I want the worker mix, so that the watchdog stays cheap.
67. As a test author, I want `--no-attach` and `--town`, so that tests do not attach a tmux client or write to `~/gt`.
68. As a test author, I want a fake agent binary to count as “on PATH”, so that tests do not need Claude or Cursor.
69. As a test author, I want the five-second limit to be asserted in tests for the local-repo case, so that a slow path cannot land.
70. As a maintainer, I want `gt install`, `gt rig add`, `gt up`, and `gt config mix` to keep working, so that `gt now` is a new front door, not a removal.
71. As a maintainer, I want `gt now` to call the same mix writer as `gt config mix`, so that there is one mix store.
72. As a maintainer, I want `gt now` not to write mix files into a random cwd when no Town exists, so that the current mix-from-/tmp bug does not return.
73. As a user of a stale `gt` binary, I want `gt now --help` to exist on a current build, so that docs and the binary match.
74. As an Overseer on a machine with two Towns, I want `gt now` to refuse to use a Dolt port that belongs to another Town’s data dir, so that Beads do not leak across Towns.
75. As an Overseer, I want `gt now` to say “you are in the Mayor session” as the success state, so that success is not “HQ created, next steps”.

## Implementation Decisions

- The only new public seam is `gt now`. Tests and docs speak to that command. Existing `gt install`, `gt rig add`, `gt config mix`, `gt up`, and `gt mayor attach` stay. `gt now` orchestrates them. It does not become a second mix store or a second Town format.
- `gt now` does not replace `gt start`. `gt start` still means “start Deacon and Mayor in an existing Town”. `gt now` means “make this git repository a working Town and put me in the Mayor session”.
- Flag shape:

  ```
  gt now [path]
    --mayor   runtime[:model[:effort]]
    --workers runtime[:model[:effort]]
    --town    path
    --name    rig-name
    --no-attach
    --restart-workers
  ```

- Parse rule for `--mayor` and `--workers`:
  - One token: runtime. Use the runtime default model. Use the group default effort (high for Mayor, low for workers).
  - Two tokens split by `:`: if the second token is a known effort for that runtime, it is effort. If not, it is model.
  - Three tokens: `runtime:model:effort`. Model may contain `/`. Model may not contain `:`.
  - This rule came from the timed mix run: aliases already hold `--model` in args; effort is a separate field. `gt now` must build that pair for the user.
- `--mayor` writes mix for roles Mayor and Deacon.
- `--workers` writes mix for roles Witness, Polecat, Refinery, Crew, Boot, and Dog.
- `gt now` creates or updates named aliases (for example `now-mayor` and `now-workers`) with `--provider` set to the runtime. It then applies mix to those aliases. The user does not run `gt config agent set`.
- Default runtime order when flags are omitted: `cursor`, `claude`, `pi`, then the first built-in runtime whose binary is on `PATH`. If none are present, fail before mutation.
- Town root: `--town`, else `$GT_TOWN_ROOT`, else `~/gt`. Never the project git repository unless that repository is already a Town HQ.
- Rig registration uses the local repository. No fetch from the network. The canonical Rig clone is this repository (or a local share of its objects). Polecat worktrees are created from that source later. Witness and Refinery directories may be created after attach.
- Dolt port: reuse the Town’s port if the Town exists. If the default port is busy with another Town, choose a free port and record it on this Town. Do not reuse another Town’s data directory.
- Start order: Dolt (or reuse) → daemon → Mayor session → attach. Deacon may start in parallel with Mayor. Witness and Refinery start after attach, or on the existing lazy path.
- Attach is the ready state. Do not block attach on Cursor `ReadyDelayMs`, formula provision, or skill copy.
- Beads must accept `gt hook` and `gt mail inbox` in the Mayor session. If full Beads init cannot fit in the budget, init the HQ Beads that those commands need before attach, and defer the rest.
- `gt now` must not call `gt config cost-tier`.
- `gt now` must not call `gt enable` or install shell integration.
- `gt now` must not use `workspace.Find` in a way that writes `settings/config.json` into a non-Town cwd. If no Town exists yet, create the Town first, then write mix inside it.
- Idempotence: a second `gt now` on the same repo finds the Town, finds the Rig, applies mix if flags changed, ensures the Mayor session, then attaches.
- Mix changes: Mayor flags restart the Mayor session. Worker flags persist for the next spawn. `--restart-workers` restarts Witness and Refinery.
- Keep `gt config mix` as the reader and the low-level writer. `gt now` is a producer of mix assignments.
- Five-second budget is a product limit for the local-repo, agent-on-PATH case. Network clone, missing CLIs, and first-time Dolt binary install are outside that limit and must fail with a clear reason or print that the limit does not apply.

## Testing Decisions

A good test observes `gt now` from the outside. It does not inspect helper functions. It checks: exit code, files a user can list, mix that `gt config mix --json` prints, tmux session presence, and wall-clock time.

Test the `gt now` command as the seam. Reuse the style of the install command tests (structure, idempotence, fail-before-mutation) and the mix tests (assignment, effort, no write on reject).

Required cases:

1. No agent CLI on `PATH`: non-zero exit, no Town created, no files in the git repository.
2. Cwd is not a git repository: non-zero exit, no Town created.
3. Happy path with a fake agent binary, `--town` in a temp dir, `--no-attach`: Town exists, Rig is registered, mix matches flags, Mayor session exists, project git repository has no `mayor/` or `deacon/` added.
4. Wall clock for case 3 is under five seconds.
5. `--mayor cursor:high --workers cursor:low` (or the fake runtime equivalent) appears in `gt config mix --json` with the right roles and effort.
6. Model form `runtime:model:effort` stores `--model` on the alias and effort on the role.
7. Invalid effort for the runtime: non-zero exit, mix unchanged.
8. Second `gt now` is idempotent and still under five seconds.
9. Default port busy with another Town: this Town gets a different port and does not attach to the other data dir.
10. `gt now` does not write mix into cwd when cwd is not a Town.
11. `gt config cost-tier` is not invoked; a Cursor mix remains a Cursor mix.
12. `--no-attach` does not attach a tmux client.

Do not require a real Cursor or Claude login in unit tests. A fake binary that exits 0 is enough to prove spawn plumbing. Full-stack tmux tests may stay behind the existing integration tag where the install tests already do that.

## Out of Scope

- Removal of `gt install`, `gt rig add`, `gt up`, `gt start`, or `gt config mix`.
- A five-second limit when the repository must be fetched from the network.
- Install of vendor CLIs (Cursor, Claude, Pi, Codex).
- Machine-wide `gt enable` and shell hooks.
- Crew member creation (`gt crew add`).
- Polecat spawn and Sling. `gt now` gets you into the Mayor session. Work dispatch stays `gt sling`.
- Federation, Wasteland, and multi-Town mesh.
- Changing Beads storage away from Dolt.
- A GUI or web dashboard start path.
- Making `gt doctor` a required step of `gt now`.
- GitHub issue tracker integration. This origin has issues disabled.

## Further Notes

GitHub issues are disabled on `jonbaldie/gastown`. This spec is the issue. Apply triage label `ready-for-agent` on the review surface that exists (this document and its pull request).

The timed run that produced this spec:

- HQ install: 7.573s
- Mix commands: about 0.02s each
- Rig add (one-file local repo): 2.004s
- Rig add (this gastown repo, local objects, `blob:none`): 13.637s
- `gt up`: 1.252s, failed, missing agent CLIs (`status 127`)
- Install with `--no-beads`: 0.034s

Those numbers are the reason `gt now` must not be a shell alias of the current five commands.

The lovely command looks like this:

```bash
cd my-repo
gt now --mayor cursor:grok-4.6:high --workers cursor:grok-4.6:low
```

Or:

```bash
gt now
```

Success is: this terminal is in the Mayor session, in a Town, with this repository as a Rig, inside five seconds.
