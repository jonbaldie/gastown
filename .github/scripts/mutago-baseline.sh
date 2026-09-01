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

jq . <<< "$matrix" > "$output_dir/matrix.json"

workers=${MUTAGO_WORKERS:-4}
total=$(jq '.include | length' <<< "$matrix")
index=0

while IFS= read -r entry; do
  index=$((index + 1))
  package=$(jq -r '.package' <<< "$entry")
  slug=$(printf '%03d-%s' "$index" "$package" | tr '/.' '__')
  if [ "$package" = "." ]; then
    package_pathspec=':(glob)*.go'
  else
    package_pathspec=":(glob)$package/*.go"
  fi
  fingerprint=$(git ls-files -s -- "$package_pathspec" .github/mutago.yml | shasum -a 256 | awk '{print $1}')
  summary="$output_dir/summaries/$slug-$fingerprint-summary.json"
  agentic="$output_dir/summaries/$slug-$fingerprint-agentic.json"
  log="$output_dir/logs/$slug-$fingerprint.log"

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

echo "All $total production Go packages meet the 80% covered-MSI requirement or are N/A."
