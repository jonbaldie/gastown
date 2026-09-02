# Agents and Rigs

Your Town is set up. Now you need two things: an **agent** to do the work, and a **Rig** — your project repo — for the agent to work on.

---

## Step 1 — Pick an agent

Gas Town works with several coding-agent CLIs. The default is **Claude** (the `claude` binary from Claude Code). Set it as your Town's default agent:

```bash
gt config default-agent claude
```

Verify:

```bash
gt config default-agent
```

You should see `claude`. That's it — every agent in your Town will use Claude unless you override it per-role.

> ### Using opencode instead?
>
> If you prefer [opencode](https://opencode.ai), configure it with a model and set it as default:
>
> ```bash
> gt config agent set opencode "opencode -m ollama-cloud/gpt-oss:120b" --provider opencode
> gt config default-agent opencode
> ```
>
> The `--provider opencode` flag tells Gas Town to use opencode's built-in preset (session handling, hooks, non-interactive mode). Swap the model string for whatever your provider offers.
>
> Gas Town also supports gemini, codex, cursor, copilot, and others. Run `gt config default-agent list` to see them all.

---

## Step 2 — Add your project as a Rig

A **Rig** is a project under Town management: one git repository and the agents that work on it. Add your existing repo:

```bash
gt rig add myproject https://github.com/yourusername/your-repo.git
```

This clones your repo into the Town and creates a structure around it:

```
myproject/
├── config.json         ← Rig configuration
├── .beads/             ← Rig-level work tracking
├── refinery/rig/       ← Canonical main clone
├── mayor/rig/          ← Mayor's working clone
├── crew/               ← Named agent workspaces (empty)
├── witness/            ← Per-Rig patrol agent
└── polecats/           ← Worker directory (empty)
```

Check that your Town sees the Rig:

```bash
gt status
```

You should see `myproject` listed under rigs.

> **Repo you don't own?** Use fork mode — Gas Town fetches upstream and pushes to your fork. See [fork-rig-setup.md](../guides/fork-rig-setup.md) for details.

---

## Step 3 — Create a Crew workspace

A **Crew** workspace is a persistent, named workspace where an agent works on your project. It's a full clone of the repo with its own branch. Create one:

```bash
gt crew add alice
```

This creates `myproject/crew/alice/` with a git clone, a mail directory for messages, and an agent instructions file.

You can add more crew members at any time:

```bash
gt crew add bob carol
```

---

## Step 4 — Start the Mayor

The **Mayor** is the Town-level coordinator. It routes work, receives escalations, and talks to you when a decision is needed. Start all Gas Town services:

```bash
gt up
```

This boots the Dolt database, the Daemon, the Deacon (health monitor), the Mayor, and per-Rig Witnesses and Refineries. It's idempotent — safe to run again.

Then enter the Mayor's session:

```bash
gt mayor attach
```

You're now inside the Mayor's tmux session, running Claude (or opencode, if you chose that). The Mayor is ready to receive work.

To detach without stopping the session: `Ctrl-B D`.

---

## What just happened

You configured an agent (Claude or opencode), added your project repo as a **Rig**, created a **Crew** workspace for an agent to work in, and started the **Mayor**. Your Town now has:

- An agent ready to receive work
- A project repo cloned and managed by Gas Town
- A named workspace (Crew) where the agent will make changes
- Background services (Mayor, Witness, Refinery) keeping everything running

The Town is alive — but there's no work to do yet. The next page creates work and shows how it reaches the agent automatically.

---

**Next:** [Creating Work →](creating-work.md)