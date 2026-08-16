# Fast `gt` builds

Daily `make build` must not compile the Dolt engine.

## Why builds were slow

`go list -deps ./cmd/gt` with CGO on is about 1400 packages. Almost all of
the extra cost is one unused import:

`github.com/steveyegge/beads` + CGO → `internal/storage/embeddeddolt` →
`github.com/dolthub/driver` → the Dolt engine, ICU (C), and cloud SDKs.

Gas Town does not call `OpenBestAvailable`. Production store opens use
`OpenFromConfig`, which talks to a Dolt SQL server over MySQL. The embedded
engine is not on that path.

A second cliff hits `go test`: `internal/testutil` used to import
testcontainers in the same package as `CleanGTEnv`. Any test that imported
testutil compiled Docker, the Dolt test module, and that module's dependency
tree.

`internal/cmd` is still one ~100k-line package. That does not change the
cold CGO graph, but it makes every command edit a full-package rebuild.

## What this fork does

| Lever | Effect |
| --- | --- |
| `CGO_ENABLED ?= 0` in the Makefile | Default `make build` / `make test` omit `embeddeddolt`. About 600 packages remain. |
| `make build-dev` | Compile only `./cmd/gt`. |
| `make build-cgo` | Old graph, in-process embedded Dolt. |
| `//go:build integration` on testutil container files | Default `go test` compiles skip stubs. `go test -tags=integration` keeps the real helpers. |
| `internal/buildgraph` | Fails CI if the CGO-off `./cmd/gt` graph grows `dolthub`, testcontainers, or go-rod. |

`go test -race` still enables CGO, so race jobs still compile the embedded
engine. That is a Go toolchain constraint, not a Gas Town one.

## What a further step change requires

1. **Shatter `internal/cmd`.** Move each command to its own package and keep
   `internal/cmd` as a thin Cobra registrar. Incremental `go build` then
   recompiles one command plus the link step.
2. **A beads client-only module.** As long as `import "github.com/steveyegge/beads"`
   compiles `storage/dolt` + CGO `embeddeddolt`, race tests and CGO release
   builds stay expensive. Types and `OpenFromConfig` should live in a module
   that does not import `dolthub/driver`.
3. **Optional OTLP / TUI tags.** After Dolt is gone, OpenTelemetry exporters
   and charmbracelet are the next largest optional subgraphs. They are not
   the current cliff.
