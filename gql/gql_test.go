// White-box pins for gql.go's entry plumbing.
package gql

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/exec"
)

// TestForceInterpThreadsThroughRun pins the dual-path harness's full
// chain (task 251): the package hook is threaded into eval.Ctx by every
// entry point and routes the executor to the interpreter, observed via
// the exec pinned counter. A dead link anywhere would leave RunBoth
// silently comparing compiled against compiled -- a differential an
// optimizer that does nothing satisfies.
func TestForceInterpThreadsThroughRun(t *testing.T) {
	g := SocialGraph(t)
	q := "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS n"

	before := exec.InterpPinned()
	forceInterp = true
	_, err := RunUncached(g, q)
	forceInterp = false
	if err != nil {
		t.Fatal(err)
	}
	if exec.InterpPinned() <= before {
		t.Fatal("forced run advanced no interpreter pins -- the dual-path hook is dead")
	}

	// The unforced leg on a native graph takes the compiled path: no pins.
	before = exec.InterpPinned()
	if _, err := RunUncached(g, q); err != nil {
		t.Fatal(err)
	}
	if exec.InterpPinned() != before {
		t.Fatal("unforced run advanced interpreter pins on a native graph")
	}
}
