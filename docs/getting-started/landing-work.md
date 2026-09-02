# Landing Work

The agent finished the work. Now the changes need to get from the Crew workspace into your main branch. That's the **Refinery**'s job.

---

## The Refinery

Every Rig has a **Refinery** — a persistent agent that runs the merge queue. The Refinery serializes all merges to main so they don't conflict. It:

1. Receives work branches from agents (submitted via `gt done`)
2. Rebases them onto the latest main
3. Runs validation (tests, builds, checks)
4. Merges to main when everything passes
5. Cleans up the branch

One Refinery per Rig. It's already running — `gt up` started it.

---

## Step 1 — The agent signals completion

When the agent finishes the work, it runs:

```bash
gt done
```

This does four things:
1. Submits the current branch to the merge queue
2. Auto-detects the Bead ID from the branch name
3. Notifies the Witness (the per-Rig patrol agent) with the outcome
4. Exits the agent session

If the agent hit a blocker and couldn't finish, it would instead run:

```bash
gt done --status ESCALATED
```

This puts the Bead back into the open queue and notifies you (the Overseer) that human intervention is needed.

> **You usually don't run `gt done` yourself.** The agent runs it when it completes the work. You'll see the result in `gt feed` or `gt status`.

---

## Step 2 — Watch the merge queue

See what's in the queue:

```bash
gt mq list
```

You'll see your work branch waiting. The Refinery processes the queue in priority order. Check the status of a specific merge request:

```bash
gt mq status
```

Or see the next item to be processed:

```bash
gt mq next
```

---

## Step 3 — The Refinery merges to main

The Refinery rebases your work branch onto the latest main, runs validation, and — if everything passes — merges it. This is automatic. You don't click merge. You don't approve a PR (unless your repo requires it on GitHub).

If validation fails (tests break, merge conflict), the Refinery spawns a **fresh Polecat** — a transient worker agent — to fix the issue. The original agent is already gone (it exited via `gt done`). The fresh Polecat re-implements the fix, resubmits, and the Refinery tries again.

> **What's a Polecat?** A Polecat is a transient worker agent with ephemeral sessions. It spawns for a specific task, does the work, and is destroyed when done. Crew agents are persistent — they stick around. Polecats are disposable. The Refinery uses them for one-off fixes like merge-conflict resolution.

---

## Step 4 — Verify the merge

After the Refinery merges, check your main branch:

```bash
cd ~/my-town/myproject/refinery/rig
git log --oneline -5
```

You should see the agent's commit at the top. The Bead is now closed:

```bash
bd show gt-abc1
```

The status should read `closed`.

---

## Your first autonomous PR

That's the full cycle. You:

1. Created a Town
2. Added an agent and your project
3. Created a Bead (work item)
4. The agent picked it up automatically (GUPP)
5. The agent ran `gt done` when finished
6. The Refinery merged it to main

You didn't dispatch the work. You didn't review the PR. You didn't click merge. You created a Bead, and Gas Town did the rest.

---

## Where to go from here

You've seen the core workflow. Here are some next steps:

- **Add more Rigs** — `gt rig add` for each project you want agents on
- **Add more Crew** — `gt crew add` for parallel agents on the same project
- **Use Convoys** — batch related Beads together for coordinated work
- **Run `gt doctor`** — check the health of your Town
- **Read the glossary** — [CONTEXT.md](../../CONTEXT.md) defines every term in the Gas Town domain
- **Explore the reference** — [docs/reference.md](../reference.md) covers config, env vars, formula format, and the full CLI

---

**Back to:** [Creating Work ←](creating-work.md) | [Agents and Rigs ←](agents-and-rigs.md) | [Start Here ←](start-here.md)