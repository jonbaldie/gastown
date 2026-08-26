#!/usr/bin/env bash
# Run an isolated, disposable Gas Town workflow against the public messrust repo.
# The script deletes the temporary Town (including all cloned worktrees) on exit.
set -euo pipefail

repo_url="${1:-https://github.com/quality-gates/messrust.git}"
town_root="$(mktemp -d "${TMPDIR:-/tmp}/gastown-messrust.XXXXXX")"
town_installed=false
cleanup() {
  # Do not invoke gt until this Town exists: a failed install could otherwise
  # resolve and shut down the caller's Town.
  if [ "$town_installed" = true ]; then
    (cd "$town_root" && gt shutdown --yes --all >/dev/null 2>&1) || true
  fi
  # Some failed shutdowns lose the Dolt PID. Kill only a server using this
  # disposable Town's own config file; never touch a server from another Town.
  while read -r dolt_pid; do
    [ -z "$dolt_pid" ] || kill -TERM "$dolt_pid" >/dev/null 2>&1 || true
  done < <(ps -ax -o pid= -o command= | awk -v config="$town_root/.dolt-data/config.yaml" 'index($0, config) { print $1 }')
  rm -rf "$town_root"
}
trap cleanup EXIT INT TERM

gt install "$town_root" --name "messrust-demo"
town_installed=true
cd "$town_root"

# Pin both the model and reasoning effort in the actual Codex invocation.
gt config agent set codex-terra-high \
  "codex -m gpt-5.6-terra -c model_reasoning_effort=high" --provider codex
gt config agent set codex-terra-medium \
  "codex -m gpt-5.6-terra -c model_reasoning_effort=medium" --provider codex
gt config mix \
  default=codex-terra-medium \
  mayor=codex-terra-high \
  deacon=codex-terra-medium \
  witness=codex-terra-medium \
  refinery=codex-terra-medium \
  polecat=codex-terra-medium \
  crew=codex-terra-medium \
  boot=codex-terra-medium \
  dog=codex-terra-medium

gt rig add messrust "$repo_url"
gt up
gt mail send mayor/ --type task --notify -s "Refactor guided by messrust" \
  -m "Please inspect the cloned messrust rig, run the repository's messrust detector on one source file, and coordinate a small, evidence-backed refactor of that one file. Do not push upstream. Report the detector findings, exact file changed, validation run, and any blocker."

printf '\nTown is running at %s. Press Enter after observing the Mayor; cleanup then runs automatically.\n' "$town_root"
read -r
