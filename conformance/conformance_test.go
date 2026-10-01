//go:build conformance

// Package conformance checks every runtime image against CONTRACT.md. For each
// runtime it builds the runtime's image and the fixture function on it
// (fixtures/<runtime>), then calls the fixture through serve and invoke.
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
}

var runtimes = []runtimeDef{
	{name: "go127", compiled: true},
}

func TestConformance(t *testing.T) {
	for _, rt := range selectedRuntimes(t) {
		t.Run(rt.name, func(t *testing.T) {
			imgs := buildImages(t, rt)
			t.Run("image", func(t *testing.T) { checkImages(t, imgs) })
			t.Run("serve", func(t *testing.T) { checkServe(t, rt, imgs) })
			t.Run("invoke", func(t *testing.T) { checkInvoke(t, rt, imgs) })
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
