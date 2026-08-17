# `gt from` — make a Town from a parent folder of local Git repositories

**Status:** ready-for-agent
**Type:** enhancement
**Category:** enhancement
**Labels:** ready-for-agent

## Test seam

There is one external seam: the `gt from` command (and the package function the command calls). Tests exercise that seam with a temporary parent folder of real Git repositories. They assert observable outcomes: Town HQ on disk, Rigs registered, source trees unchanged.

The command reuses Town install and Rig add internally. Tests do not mock those steps. They do not add a second public seam.

---

## Problem Statement

I have a parent folder. That folder contains many local Git repositories. The services in those repositories talk to each other at run time.

I want a Gas Town for that layout. I want one Rig per repository. I want the original folders left as they are. I want Compose to keep running from the parent folder.

Today I must run many commands. I must create a Town HQ at a path that is not the parent folder. I must add each repository as a Rig. For each Rig I must choose a Git URL, pass a local-repo reference, pick a legal Rig name, and pick a Beads prefix. I must remember that adopt means “register an assembled Rig directory,” not “this folder is my project.” I must remember that a local-repo reference requires the origin URL to match the Git URL. I must remember that Git may block file URLs.

I want one command that covers the usual local cases: no remote, HTTPS origin, SSH origin, mixed children, hyphenated folder names, leftover Compose files, and a Town that already exists.

## Solution

Add a command:

```text
gt from <parent> [town]
gt from <parent> --dry-run
```

`<parent>` is the parent folder of Git repositories. It is not the Town HQ.

The command makes a Town next to that folder. The default HQ path is the sibling `<parent>.gt`. The optional `[town]` argument sets a different HQ path, such as `~/gt`.

The command finds each immediate child that is a Git repository. It makes one Rig per child. It uses the child’s origin when origin exists. It uses a local file URL when origin does not exist. It always uses a local-repo reference. It never uses adopt.

The command does not change the original repositories. It does not start the Mayor. It does not create a Crew member. It does not create a Convoy. It does not enable Gas Town on the machine.

A second run adds only new children. It does not duplicate Rigs that already match.

## User Stories

1. As a developer with a parent folder of local Git repositories, I want one command that makes a Town from that folder, so that I do not learn the install and Rig-add ceremony.
2. As a developer, I want the first argument to be the parent folder of repositories, so that I do not confuse it with the Town HQ path on install.
3. As a developer, I want the Town to sit next to the parent folder by default, so that the parent folder does not become HQ.
4. As a developer, I want to pass an optional Town path, so that I can use my existing `~/gt` Town when I choose to.
5. As a developer, I want `gt from ~/code` to refuse a Town path inside the parent folder, so that HQ files do not mix with my services.
6. As a developer, I want each immediate child Git repository to become one Rig, so that each project has its own Witness, Refinery, Beads prefix, and workers.
7. As a child repository with an HTTPS origin, I want that origin used as the Rig Git URL, so that fetch and push keep the hosted remote.
8. As a child repository with an SSH origin, I want that origin used as the Rig Git URL, so that SSH remotes keep working.
9. As a child repository with no remotes, I want a local file URL plus a local-repo reference, so that the Rig still clones without a hosted remote.
10. As a parent folder with mixed remotes, I want each child classified on its own, so that one HTTPS child does not force file URLs on the others.
11. As a developer, I want the original working trees left unchanged, so that Compose and my editor keep using the parent folder.
12. As a developer, I want local-repo object sharing, so that clones of large local repositories stay fast.
13. As a developer, I want the command never to use adopt, so that a project folder is not treated as an assembled Rig directory.
14. As a folder named `my-app`, I want a Rig named `my_app`, so that the Rig name meets the letters-digits-underscore rule.
15. As two folders that sanitize to the same Rig name, I want the command to fail before it writes, so that one child does not overwrite the other.
16. As a folder that sanitizes to the reserved name `hq`, I want the command to refuse that child, so that Town HQ identity stays distinct from Rigs.
17. As a developer with leftover `compose.yaml` in the parent folder, I want that file reported and not turned into a Rig, so that non-Git glue does not become a false project.
18. As a developer with non-Git subdirectories, I want those directories skipped, so that docs folders and build output do not become Rigs.
19. As a developer with hidden directories, I want dot-directories skipped, so that `.git` and tool caches are not scanned as children.
20. As a parent folder that is itself a Git repository and that has Git children, I want only the children to become Rigs, so that the container repository is not a fifth project.
21. As a parent folder that is itself a Git repository and that has no Git children, I want one Rig from that parent, so that a single local repo still works.
22. As a parent folder with no Git repositories at all, I want a clear error and no Town writes, so that I can see that the folder was the wrong target.
23. As a developer who already has a Town at the target HQ path, I want the command to reuse that Town, so that I do not reinstall HQ.
24. As a developer whose target HQ path exists but is not a Town, I want a clear error, so that the command does not install into a random directory.
25. As a developer who runs the command twice, I want existing matching Rigs skipped, so that the command is safe to repeat.
26. As a developer who adds a new child repository later, I want a second run to add only that Rig, so that the Town grows with the parent folder.
27. As a developer, I want a skip when the registered Rig name already points at the same local path, so that identity stays stable.
28. As a developer, I want a conflict when a Rig name is taken by a different local path, so that I do not silently attach the wrong repository.
29. As a developer, I want `--dry-run` to print the planned Town path and Rigs without writing, so that I can check names and URLs first.
30. As a developer, I want preflight errors to happen before any write, so that a name collision does not leave a half-made Town.
31. As a developer, I want a failed Rig add to leave successful Rigs in place, so that I can run the command again to finish.
32. As a developer, I want Beads prefixes derived from Rig names, so that I do not pick prefixes by hand.
33. As a developer, I want prefix collisions detected, so that two Rigs do not share one Beads prefix.
34. As a developer, I want the default branch detected from the child, so that I do not pass a branch flag.
35. As a developer, I want Git file-protocol clones to work without setting Git config myself, so that local-only children clone on modern Git.
36. As a developer, I want the new Town created with Git history for HQ and without machine-wide shell enablement, so that a test Town does not take over the host.
37. As a developer, I want Dolt started only as Town install already starts it, so that Beads work without a second service command.
38. As a developer, I want the Mayor left stopped, so that a first look at the layout does not start agents.
39. As a developer, I want no Crew member created, so that “make the Town” stays separate from “make my workspace.”
40. As a developer, I want no Convoy created, so that empty work is not tracked.
41. As a developer, I want no `gt up`, so that Witness and Refinery stay stopped until I start them.
42. As a developer, I want the command to refuse to run when the current directory is already inside a Town if that would scan HQ as if it were a parent of repositories, so that I do not import Rig containers as source repos.
43. As a developer, I want directories that already look like assembled Rigs skipped, so that a Town output folder is not imported as a source.
44. As a developer, I want a report of Town created or reused, Rigs added, Rigs skipped, glue ignored, and failures, so that I can see what happened.
45. As a developer, I want `gt from --help` to say that the first path is the parent of repositories and that install’s path is HQ, so that the two commands stay distinct.
46. As a developer, I want install’s meaning unchanged, so that existing scripts that pass an HQ path keep working.
47. As a developer, I want `gt rig add` unchanged for one-repo hosted clones, so that the deep command does not replace the precise command.
48. As a developer, I want `gt rig add --adopt` unchanged, so that assembled Rig directories still register the old way.
49. As a developer, I want the hidden one-repo quick-add path left for the shell hook, so that “add this repo?” still works for a single project with a remote.
50. As a developer on this fork, I want local-only children to work even when quick-add would refuse them for missing remotes, so that the new command covers the case the hook cannot.
51. As a developer, I want fork push-url and upstream-url left on `gt rig add`, so that third-party remotes stay an explicit later step.
52. As a developer, I want nested Git repositories below the first child level ignored, so that vendor clones and submodules are not imported by surprise.
53. As a developer, I want relative parent paths resolved from the current directory, so that `gt from .` and `gt from ./services` work.
54. As a developer, I want the default Town sibling named `<parent>.gt`, so that `/tmp/demo` becomes `/tmp/demo.gt` and does not collide with a child named `gt`.
55. As a developer, I want `--dry-run` to use the same discovery rules as a real run, so that the printed plan matches what a write would do.
56. As a test author, I want to build a temporary parent of fake Git repositories and run `gt from`, so that I can assert the Town and Rigs without a hosted remote.
57. As a test author, I want to assert the child’s `HEAD` is unchanged after `gt from`, so that source-tree safety is observable.
58. As a test author, I want to assert `compose.yaml` in the parent is not a Rig, so that glue handling is observable.
59. As a test author, I want a second `gt from` to add zero Rigs when nothing changed, so that idempotence is observable.
60. As a maintainer, I want this behaviour documented next to local Rig bootstrap, so that `--adopt` is not recommended for project folders.
61. As a maintainer, I want the command to call install and Rig add as implementation, so that HQ layout and Rig layout stay one source of truth.
62. As a maintainer, I want file-protocol allowance in the Git clone path, so that every local file URL clone benefits, not only this command.
63. As a developer, I want a dry-run exit of success when the plan is valid, so that scripts can inspect before they write.
64. As a developer, I want a non-zero exit when preflight fails or when any Rig fails to add, so that automation can detect partial work.
65. As a developer, I want Beads databases for new Rigs initialized the same way Rig add already does, so that `bd` works in each Rig after the command.
66. As a developer, I want namepool themes assigned the same way Rig add already does, so that Polecat names stay unique across Rigs.
67. As a developer, I want no change to how services talk at run time, so that Compose in the parent folder remains the runtime workspace.
68. As a developer, I want a printed reminder that Compose sibling paths do not exist inside a Rig clone, so that I do not try to start the stack from HQ.
69. As a new Gas Town user, I want this command to be the first step for a local polyrepo, so that I can see Town, Rigs, and Beads before I start agents.
70. As an agent implementing this spec, I want the external behaviour listed here to be the contract, so that I do not invent flags for Crew, Convoy, origin policy, or include/exclude.

## Implementation Decisions

- Add one top-level command, a peer of install, named `from`. Do not add `gt town from`. The existing `gt town` command is session cycling.
- Do not add `--from` to install. Install’s path argument remains Town HQ.
- Do not add `--scan` to Rig add. Rig add remains one repository. The new command calls it.
- Command shape:

```text
gt from <parent> [town]
gt from <parent> --dry-run
```

- `<parent>` is required. `[town]` is optional. `--dry-run` is the only flag on the default path.
- Default Town path is the sibling of `<parent>` named `<basename(parent)>.gt`.
- If `[town]` resolves inside `<parent>`, fail before writes.
- If `[town]` exists and is already a Town, reuse it.
- If `[town]` exists and is not a Town, fail.
- Discovery scans only immediate children of `<parent>`. A child is a candidate when it is a directory, is not dot-prefixed, and is a Git repository with a `.git` directory. Git files that mark submodules are not candidates.
- If `<parent>` is a Git repository and has no candidate children, the parent itself is the single candidate.
- If `<parent>` is a Git repository and has candidate children, only the children are candidates.
- Leftover files such as `compose.yaml` are listed in the report. They are not Rigs. The command does not create a platform repository.
- Rig names come from the existing name sanitizer used by one-repo quick-add: hyphen, dot, and space become underscore.
- Two candidates that sanitize to the same name are a preflight error.
- A sanitized name of `hq` is a preflight error for that candidate.
- Git URL policy: if `origin` exists, use that URL exactly, and pass the child path as the local-repo reference so the existing origin-match rule succeeds. If `origin` does not exist, use a `file://` URL of the absolute child path and still pass the local-repo reference.
- Never pass adopt.
- Never rewrite remotes in the source working tree.
- New Towns are installed with Git for HQ and without shell enablement.
- The command does not create Crew, does not create a Convoy, does not start the Mayor, and does not run `gt up`.
- Internally, discovery may return a plan value and apply may consume that plan. That split is an internal seam for `--dry-run`. It is not a second user command.
- Call Town install and Rig add through in-process helpers. Do not shell out to `gt`.
- When the Git clone URL is `file://`, the Git clone helper must allow the file protocol so Git 2.38 and later can clone. That change lives in the Git helper so all local clones benefit.
- Reuse existing prefix derivation, default-branch detection, and namepool theme assignment from Rig add.
- Idempotence: skip a candidate when a Rig of that name is already registered with the same local-repo path or the same Git URL. Conflict when the name exists with a different source.
- Partial apply: after a successful preflight, continue remaining Rigs if one add fails. Exit non-zero if any add failed.
- `--dry-run` runs discovery and prints the plan. It does not install. It does not add Rigs.
- The hidden one-repo quick-add command stays for the shell hook. It still requires a remote. It is not the polyrepo command.
- Local Rig bootstrap documentation must say: for a parent folder of project repositories, use `gt from`. Use adopt only for an assembled Rig directory. Use the bootstrap script for one local repository when you already have a Town.

## Testing Decisions

A good test asserts what a caller can observe after `gt from`: Town HQ exists or was reused, `rigs.json` lists the expected Rigs with the expected Git URLs and local-repo paths, Beads prefixes exist, the source repositories’ `HEAD` and working trees are unchanged, leftover glue is not a Rig, and a second run does not duplicate Rigs. Tests do not assert helper call counts. Tests do not inspect private functions.

The module under test is the `from` command at its interface. Install, Rig add, and Git clone run for real against temporary directories.

Prior art:

- Install end-to-end tests that run the `gt` binary against a temp HQ and assert `town.json` and `rigs.json`.
- Rig add integration tests that create a temp Git repository, add a Rig, and assert the Rig container shape.
- Rig add tests that already allow `file://` URLs.
- Beads init tests that cover adopt as a different path. This feature must not use that path.
- Git tests that currently set file-protocol allow only in test helpers. Production clone must gain the same allow for `file://` so the new command’s local-only case passes outside those tests.

Required observable cases at the seam:

- Mixed children: HTTPS origin, SSH origin, no origin.
- Hyphenated directory name becomes a legal Rig name.
- Leftover `compose.yaml` is not a Rig.
- Default Town path is the sibling `*.gt` and is not inside the parent.
- Custom Town path reuses an existing Town.
- `--dry-run` writes nothing.
- Second run is idempotent.
- Parent-as-only-repo makes one Rig.
- Parent-as-git-with-children uses children only.
- Empty parent fails with no writes.
- Source `HEAD` unchanged.

Prefer the same isolated Dolt-port pattern the install tests already use.

## Out of Scope

- Crew creation
- Convoy creation
- Starting the Mayor, Deacon, Witness, Refinery, or daemon
- Machine-wide shell enablement
- Include and exclude globs
- Recursive nested scan
- Origin policy flags (force all file URLs, or require remotes)
- Automatic platform Rig from leftover Compose files
- Fork routing (push URL and upstream URL)
- Per-Rig prefix, branch, or agent flags
- Changing how services talk at run time
- Changing install’s path meaning
- Changing adopt’s meaning
- Replacing one-repo quick-add for the shell hook
- Importing a parent folder that is already a Town HQ as if it were a source tree

## Further Notes

This fork has GitHub issues disabled. This spec is the issue. Status is ready-for-agent.

The runtime workspace stays the parent folder. The Town is HQ for agents and Beads. A Rig clone does not contain sibling repositories. Compose that uses `../auth` still belongs in the parent folder.

`gt from` is the deep module. Install plus repeated Rig add is the shallow cluster it hides.
