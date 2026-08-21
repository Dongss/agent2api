//go:build conformance

package main

// The scripted mock backend, for driving the API surface against outcomes no
// real CLI can be asked for on demand. See internal/adapter/mock.
import _ "github.com/Dongss/agent2api/internal/adapter/mock"
