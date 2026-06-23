// Package integration holds CairnOS's live-Postgres tests. They build and run
// only under the `integration` build tag; the Makefile `test` and `integration`
// targets supply the container runtime. This file carries the package clause for
// the default (untagged) build so `go build ./...` has something to compile when
// the tag is off; the real support code lives in support.go behind the tag.
package integration
