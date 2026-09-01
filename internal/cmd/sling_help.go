package cmd

const slingCommandLong = `Sling work onto an agent's hook and start working immediately.

This is THE command for assigning work in Gas Town. It handles:
  - Existing agents (mayor, crew, witness, refinery)
  - Auto-spawning polecats when target is a rig
  - Dispatching to dogs (Deacon's helper workers)
  - Formula instantiation and wisp creation
  - Auto-convoy creation for dashboard visibility

Create work in the target rig:
  File the bead where the code lives. Do not create town hq-* work beads
  and sling them to a rig.

  bd -C <town>/<rig> create --title=... --type=feature

  That directory has its own .beads with the rig prefix (gt from and
  gt rig add create it). If it is missing, bd walks up to town beads
  and silently files hq-* work.
  gt convoy create "..." <rig-prefix-id>
  gt convoy add <convoy-id> <rig-prefix-id>
  gt sling <rig-prefix-id> <rig> --merge=local   # third-party remotes

  Do not sling infrastructure beads (hq-cv-*, hq-mayor, ...). Those stay at town.
  Use --merge=local (or --push-url) for third-party public clones.

Auto-Convoy:
  When slinging a single issue (not a formula), sling automatically creates
  a convoy to track the work unless --no-convoy is specified. This ensures
  all work appears in 'gt convoy list', even "swarm of one" assignments.

  gt sling gt-abc gastown              # Creates "Work: <issue-title>" convoy
  gt sling gt-abc gastown --no-convoy  # Skip auto-convoy creation

Town Beads:
  Mayor-created town work (hq-*) can be slung to a rig. Sling moves the bead
  into the target rig with the same path as "gt bead move", then dispatches
  the new rig-prefixed ID. Convoys (hq-cv-*) and town agent/channel/group
  beads stay in HQ and are not auto-moved.

Merge Strategy (--merge):
  Controls how completed work lands. Stored on the auto-convoy and on the issue.
  gt sling gt-abc gastown --merge=direct  # Push branch directly to main
  gt sling gt-abc gastown --merge=mr      # Merge queue (default)
  gt sling gt-abc gastown --merge=local   # Keep on feature branch

  An explicit --merge value wins. If --merge is omitted, sling reuses a stored
  merge_strategy on the issue. If the issue text says "local commit only" or
  "do not push", sling uses local and stores merge_strategy=local so later
  dispatches keep the policy without the flag.

Target Resolution:
  gt sling gt-abc                       # Self (current agent)
  gt sling gt-abc crew                  # Crew worker in current rig
  gt sling gp-abc greenplace               # Auto-spawn polecat in rig
  gt sling gt-abc greenplace/Toast         # Specific polecat
  gt sling gt-abc gastown --crew mel    # Crew member mel in gastown
  gt sling gt-abc mayor                 # Mayor
  gt sling gt-abc deacon/dogs           # Auto-dispatch to idle dog
  gt sling gt-abc deacon/dogs/alpha     # Specific dog

Spawning Options (when target is a rig):
  gt sling gp-abc greenplace --create               # Create polecat if missing
  gt sling gp-abc greenplace --force                # Ignore unread mail
  gt sling gp-abc greenplace --account work         # Use specific Claude account

Natural Language Args:
  gt sling gt-abc --args "patch release"
  gt sling code-review --args "focus on security"

The --args string is stored in the bead and shown via gt prime. Since the
executor is an LLM, it interprets these instructions naturally.

Stdin Mode (for shell-quoting-safe multi-line content):
  echo "review for security issues" | gt sling gt-abc gastown --stdin
  gt sling gt-abc gastown --stdin <<'EOF'
  Focus on:
  1. SQL injection in query builders
  2. XSS in template rendering
  EOF

  # With --args on CLI, stdin goes to --message:
  echo "Extra context here" | gt sling gt-abc gastown --args "patch release" --stdin

Formula Slinging:
  gt sling mol-release mayor/           # Cook + wisp + attach + nudge
  gt sling towers-of-hanoi --var disks=3

Formula-on-Bead (--on flag):
  gt sling mol-review --on gt-abc       # Apply formula to existing work
  gt sling shiny --on gt-abc crew       # Apply formula, sling to crew

Compare:
  gt hook <bead>      # Just attach (no action)
  gt sling <bead>     # Attach + start now (keep context)
  gt handoff <bead>   # Attach + restart (fresh context)

The propulsion principle: if it's on your hook, YOU RUN IT.

Batch Slinging:
  gt sling gt-abc gt-def gt-ghi gastown   # Sling multiple beads to a rig
  gt sling gt-abc gt-def gastown --max-concurrent 3  # Spawn 3 at a time

  When multiple beads are provided with a rig target, each bead gets its own
  polecat. This parallelizes work dispatch without running gt sling N times.
  Use --max-concurrent to throttle spawn rate and prevent Dolt server overload.`
