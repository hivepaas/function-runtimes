//go:build conformance

// Package conformance checks every runtime image against CONTRACT.md. For each
// runtime it builds the runtime's image and the fixture function on it
// (fixtures/<runtime>, or the one its language shares), then calls the fixture through serve and invoke.
//
// It needs Docker, and Docker Hub for the base images:
//
//	go test -tags conformance ./conformance/
//	RUNTIMES=node24 go test -tags conformance ./conformance/
//
// The images it builds are removed afterwards, unless KEEP_IMAGES is set.
package conformance

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// runtimeDef is a runtime of runtimes/.
type runtimeDef struct {
	name string
	// compiled runtimes build the function in a build image of their own, and
	// load nothing when they start: a handler that is missing fails the build.
	compiled bool
	// typeScript is how the runtime runs a TypeScript handler.
	typeScript typeScript
	// processes: the runtime serves with several processes, as many as
	// WEB_CONCURRENCY says; serve's checks run with several and with one.
	processes bool
	// closesHTTP10: the runtime closes an HTTP/1.0 connection after its
	// answer, which says so, even when the request asks to keep it - Bun's
	// node:http does.
	closesHTTP10 bool
	// fixture is the directory of fixtures/ the runtime's fixture is built
	// from, when it is not the runtime's name: runtimes of one language share
	// it.
	fixture string
}

func (rt runtimeDef) fixtureName() string {
	if rt.fixture != "" {
		return rt.fixture
	}
	return rt.name
}

var runtimes = []runtimeDef{
	{name: "go127", compiled: true},
	{name: "node24", typeScript: typesRemoved, fixture: "js"},
	{name: "bun1", typeScript: wholeTypeScript, closesHTTP10: true, fixture: "js"},
	{name: "python313", processes: true},
}

func TestConformance(t *testing.T) {
	for _, rt := range selectedRuntimes(t) {
		t.Run(rt.name, func(t *testing.T) {
			imgs := buildImages(t, rt)
			t.Run("image", func(t *testing.T) { checkImages(t, imgs) })
			t.Run("serve", func(t *testing.T) { checkServe(t, rt, imgs) })
			if rt.processes {
				one := imgs
				one.env = []string{"WEB_CONCURRENCY=1"}
				t.Run("serve, one process", func(t *testing.T) { checkServe(t, rt, one) })
				t.Run("processes", func(t *testing.T) { checkProcesses(t, imgs) })
			}
			t.Run("invoke", func(t *testing.T) { checkInvoke(t, rt, imgs) })
			if rt.typeScript != noTypeScript {
				t.Run("typescript", func(t *testing.T) { checkTypeScript(t, rt, imgs) })
			}
		})
	}
}

// selectedRuntimes are the runtimes RUNTIMES names, comma separated; all when
// it is empty.
func selectedRuntimes(t *testing.T) []runtimeDef {
	names := strings.FieldsFunc(os.Getenv("RUNTIMES"), func(r rune) bool { return r == ',' || r == ' ' })
	if len(names) == 0 {
		return runtimes
	}
	var selected []runtimeDef
	for _, rt := range runtimes {
		if slices.Contains(names, rt.name) {
			selected = append(selected, rt)
		}
	}
	if len(selected) != len(names) {
		t.Fatalf("RUNTIMES=%q names a runtime that is not one of %v", os.Getenv("RUNTIMES"), runtimes)
	}
	return selected
}
