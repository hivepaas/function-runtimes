//go:build conformance

package conformance

import (
	"strings"
	"testing"
)

// typeScript is how a runtime runs TypeScript.
type typeScript int

const (
	// noTypeScript: the runtime runs no TypeScript.
	noTypeScript typeScript = iota
	// typesRemoved: the types are removed as a file loads, and syntax that
	// cannot be removed - an enum - cannot be loaded.
	typesRemoved
)

// checkTypeScript calls the fixture's TypeScript handler, typed.ts, through
// serve and invoke: it imports a file of its own and a type, with their
// extensions.
func checkTypeScript(t *testing.T, rt runtimeDef, imgs images) {
	const want = `{"hello":"Ada"}`

	t.Run("a TypeScript handler, served", func(t *testing.T) {
		c := startServe(t, imgs.fixture, "HP_FN_ENTRYPOINT=typed.ts")

		if r := c.get(t, "/?name=Ada"); r.status != 200 || !sameBody(r.body, []byte(want)) {
			t.Errorf("%d %q", r.status, r.body)
		}
	})

	t.Run("a TypeScript handler, invoked", func(t *testing.T) {
		r := invoke(t, imgs.fixture, `{"query":{"name":["Ada"]}}`, "HP_FN_ENTRYPOINT=typed.ts")

		if res := parseResult(t, r); res.Status != 200 || !sameBody(res.body(t), []byte(want)) {
			t.Errorf("result %+v\n%s", res, lastLines(r.stderr))
		}
	})

	if rt.typeScript == typesRemoved {
		t.Run("an enum cannot be loaded: the types are only removed", func(t *testing.T) {
			r := invoke(t, imgs.fixture, `{}`, "HP_FN_ENTRYPOINT=enum.ts")

			// Node.js names the syntax it cannot remove.
			if r.code != 3 || !strings.Contains(r.stderr, "the handler could not be loaded") ||
				!strings.Contains(r.stderr, "ERR_UNSUPPORTED_TYPESCRIPT_SYNTAX") {
				t.Errorf("exit %d\n%s", r.code, lastLines(r.stderr))
			}
		})
	}
}
