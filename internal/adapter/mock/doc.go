// Package mock is a scripted backend, compiled in only with the "conformance"
// build tag.
//
// It drives the API surface against every outcome a real CLI can produce,
// including ones nobody can summon on demand like a rate limit or a crash,
// without a CLI, an account or a token spent. The model variant picks the
// behaviour: mock:ok, mock:rate-limited, and so on. See mock.go for the scripts
// and scripts/conformance for the suite.
//
// A normal build leaves it out: a gateway that ships a backend which fabricates
// answers can lie to its user.
package mock
