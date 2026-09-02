# First-run does not compile a database

An Overseer gets `gt` and `bd` with `go install`, then creates a Town with `gt install`. Those commands must succeed without extra compiler flags or headers; Dolt is a separate program they install, and a Town uses that. First-run must not compile a database, because teaching a compiler flag on every command is not the product.
