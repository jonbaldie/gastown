# Creating Work

Your Town has an agent, a project, and a workspace. Now you need to give the agent something to do.

In Gas Town, work is tracked as **Beads**. A Bead is the atomic unit of tracked work — a bug fix, a feature, a chore. When a Bead lands on an agent's **Hook**, the agent runs it. This happens automatically through a rule called **GUPP**.

---

## Step 1 — Create a Bead

Create a Bead with `bd` (the Beads CLI, bundled with Gas Town):

```bash
bd create --title="Fix login redirect bug" --type=bug --priority=1
```

You'll see something like:

```
✓ Created issue: gt-abc1 — Fix login redirect bug
```

That `gt-abc1` is the Bead ID. It's how you'll refer to this work from now on.

Add more detail if you want:

```bash
bd update gt-abc1 --description="After login, users hit a 404 instead of the dashboard. Reproduce by logging in with a test account."
```

> **What is a Bead, exactly?** A Bead is the work record itself — not the work. Think of it as a ticket that lives in a database (Dolt). It has a title, a type (bug, feature, task, chore), a priority, and a status. Agents read Beads, do the work, and update the status. You'll never lose track of a Bead — it survives session restarts, crashes, and context compaction.

---

## Step 2 — See what's ready

Check all ready work across your Town:

```bash
gt ready
```

This shows every Bead with no blockers, sorted by priority. Your `gt-abc1` should be at the top.

---

## Step 3 — Put the Bead on a Hook

A **Hook** is a Bead pinned to an agent as its current assignment. When a Bead is on an agent's Hook, that agent is responsible for it.

Attach the Bead to your Crew agent's Hook:

```bash
gt hook gt-abc1 myproject/crew/alice
```

Or, if you're inside the agent's session (e.g., you ran `gt mayor attach` and are acting as that agent):

```bash
gt hook gt-abc1
```

Check what's on the Hook:

```bash
gt hook
```

---

## Step 4 — The agent runs it (GUPP)

Here's where Gas Town does something most tools don't: **the agent picks up the work automatically.** You don't dispatch it. You don't click "run." The agent sees the Bead on its Hook and starts working.

This is **GUPP** — the rule that a Role with work on its Hook runs that work. It's short for "Gas Town Universal Propulsion Principle," but the name matters less than the effect: work on a Hook gets done.

If you're attached to the Mayor's session, you'll see the agent start working on the Bead. It'll read the description, explore the codebase, make changes, and run tests — just like a developer would.

> **Why GUPP?** Most orchestration tools require you to explicitly dispatch each task. Gas Town inverts this: the default is that work runs. You create a Bead, it lands on a Hook, the agent runs it. You only intervene when something goes wrong — an escalation, a blocked dependency, a review needed.

---

## Step 5 — Check on progress

See the current state of your Town:

```bash
gt status
```

Or watch a live feed of agent activity:

```bash
gt feed
```

The feed shows what each agent is doing in real time. You'll see your Crew agent working on the Bead, making changes, and eventually signalling completion.

---

## What just happened

You created a **Bead** (the work record), put it on an agent's **Hook** (the pinned assignment), and the agent started working on it automatically thanks to **GUPP** (the rule that work on a Hook runs).

The agent is now making changes to your project in its Crew workspace. When it's done, it'll signal completion — and the work needs to land in your main branch. That's the Refinery's job.

---

**Next:** [Landing Work →](landing-work.md)