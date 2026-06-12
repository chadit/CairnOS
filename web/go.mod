// The web SPA is a TypeScript/Vite project, not Go. This nested module exists
// only to prune web/ from the root module's `./...` walk: npm dependencies
// (e.g. flatted) ship stray .go sample files under node_modules, and without a
// module boundary here those files get absorbed into github.com/chadit/CairnOS
// and pollute `go build`, `go test`, and `golangci-lint run`. No Go code in
// this tree is meant to build.
module github.com/chadit/CairnOS/web

go 1.26
