# Start Here

You have a repository with more work than hands. Bug fixes piling up, features waiting in the backlog, PRs collecting dust. Gas Town gives you a Town full of autonomous agents that pick up work, run it, and ship PRs — without you dispatching every task.

This guide walks you from zero to your first autonomous PR. It takes about 15 minutes if you already have a git repo.

---

## Step 1 — Install `gt`

Gas Town is a Go CLI called `gt`. Install it with:

```bash
go install github.com/jonbaldie/gastown/cmd/gt@latest
```

You'll need Go 1.26 or later. Verify the install:

```bash
gt --version
```

If `gt` isn't on your `PATH`, make sure `$(go env GOPATH)/bin` is included. On most systems:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

> **Building from source?** Clone this repo and run `make build-dev`. That produces a `gt` binary you can copy to `~/go/bin/`.

---

## Step 2 — Create a Town

A **Town** is the outermost workspace. It holds your projects (called **Rigs**) and the agents that work on them. Create one:

```bash
gt install ~/my-town
```

This creates a directory structure that looks like:

```
~/my-town/
├── AGENTS.md          ← Town identity (CLAUDE.md symlinks here)
├── mayor/             ← Mayor config, rig registry, town settings
├── .beads/            ← Work-tracking database (Beads)
└── CONTEXT.md         ← Domain glossary
```

Move into your Town:

```bash
cd ~/my-town
```

---

## What just happened

You installed `gt`, the Gas Town CLI. Then you created a **Town** — the container that will hold your projects and the agents that work on them.

Right now your Town is empty. No projects, no agents, no work. The next page adds an agent and your first project.

---

**Next:** [Agents and Rigs →](agents-and-rigs.md)