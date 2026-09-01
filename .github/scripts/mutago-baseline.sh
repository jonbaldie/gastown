#!/usr/bin/env bash

set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 OUTPUT_DIRECTORY" >&2
  exit 2
fi

output_dir=$1
repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

mkdir -p "$output_dir/logs" "$output_dir/summaries"
output_dir=$(cd "$output_dir" && pwd)

if [ -e mutago-summary.json ] || [ -e mutago-agentic.json ]; then
  echo "Refusing to overwrite an existing Mutago report in $repo_root." >&2
  exit 2
fi

revision=$(git rev-parse HEAD)
config_sha=$(shasum -a 256 .github/mutago.yml | awk '{print $1}')
metadata="$revision $config_sha"

if [ -f "$output_dir/metadata" ]; then
  if [ "$(cat "$output_dir/metadata")" != "$metadata" ]; then
    echo "The saved baseline belongs to a different revision or Mutago config." >&2
    exit 2
  fi
else
  printf '%s\n' "$metadata" > "$output_dir/metadata"
fi

matrix=$(git ls-files -z -- '*.go' ':(exclude)*_test.go' |
  jq -Rsc '
    def package:
      (split("/") | .[0:-1] | join("/")) |
      if . == "" then "." else . end;
    split("\u0000") |
    map(select(length > 0)) |
    sort_by(package) |
    group_by(package) |
    map({package: (.[0] | package), files: .}) |
    sort_by([(.files | length), .package]) |
    {include: .}
  ')

if [ -f "$output_dir/matrix.json" ]; then
  if [ "$(jq -cS . "$output_dir/matrix.json")" != "$(jq -cS . <<< "$matrix")" ]; then
    echo "The saved baseline has a different production-file matrix." >&2
    exit 2
  fi
else
  jq . <<< "$matrix" > "$output_dir/matrix.json"
fi

workers=${MUTAGO_WORKERS:-4}
total=$(jq '.include | length' <<< "$matrix")
index=0

while IFS= read -r entry; do
  index=$((index + 1))
  package=$(jq -r '.package' <<< "$entry")
  slug=$(printf '%03d-%s' "$index" "$package" | tr '/.' '__')
  summary="$output_dir/summaries/$slug-summary.json"
  agentic="$output_dir/summaries/$slug-agentic.json"
  log="$output_dir/logs/$slug.log"

  if [ -f "$summary" ]; then
    echo "[$index/$total] checking saved result: $package"
  else
    files=()
    while IFS= read -r file; do
      files+=("$file")
    done < <(jq -r '.files[]' <<< "$entry")

    echo "[$index/$total] mutating: $package (${#files[@]} files)"
    mutago \
      --config .github/mutago.yml \
      --coverage \
      --per-test \
      --workers "$workers" \
      --test-flags=-short \
      --timeout-coefficient 3 \
      --logger-summary-json \
      --logger-agentic-json \
      --quiet \
      --no-diffs \
      "${files[@]}" > "$log" 2>&1

    if [ ! -f mutago-summary.json ] || [ ! -f mutago-agentic.json ]; then
      echo "Mutago did not produce both reports for $package; see $log." >&2
      exit 1
    fi
    mv mutago-summary.json "$summary"
    mv mutago-agentic.json "$agentic"
  fi

  read -r killed covered total_mutants < <(jq -r '
    [
      .killedCount,
      .totalMutantsCount - .notCoveredCount,
      .totalMutantsCount
    ] | @tsv
  ' "$summary")
  if [ "$covered" -eq 0 ]; then
    echo "[$index/$total] N/A: $package has no covered mutants ($total_mutants total)"
  elif [ "$((killed * 100))" -lt "$((covered * 80))" ]; then
    awk -v package="$package" -v killed="$killed" -v covered="$covered" \
      'BEGIN { printf "%s covered-MSI: %.2f%% (%d/%d)\n", package, 100 * killed / covered, killed, covered }'
    echo "$package is below the required 80%; see $agentic." >&2
    exit 4
  else
    awk -v package="$package" -v killed="$killed" -v covered="$covered" \
      'BEGIN { printf "%s covered-MSI: %.2f%% (%d/%d)\n", package, 100 * killed / covered, killed, covered }'
  fi
done < <(jq -c '.include[]' <<< "$matrix")

summary_count=$(find "$output_dir/summaries" -name '*-summary.json' -type f | wc -l | tr -d ' ')
if [ "$summary_count" -ne "$total" ]; then
  echo "Expected $total summaries, found $summary_count." >&2
  exit 1
fi

echo "All $total production Go packages meet the 80% covered-MSI requirement or are N/A."
