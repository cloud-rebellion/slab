package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// Coverage guard for the reservation-release paths.
//
// The 2026-08-11 audit's cross-cutting finding was that a guard applied to one
// surface and not its twin accounts for most of this codebase's defects, and
// that "fixing instances will not fix it". This package is the proof: the
// release-the-reservation logic exists on four paths, the audit counted the bug
// on two of them, and the fix pass found a third (the MCP
// UpdateStatusForAgent). Reviewer memory is not closing this class.
//
// So this test enumerates the call sites from the AST rather than from a
// hand-written list. A fifth release path added next year is covered on the day
// it is written, with no reviewer needing to remember this rule, which is the
// property the audit asked for and the property `retention/coverage_test.go`
// (a hand-maintained map of nine tables) does not have.
//
// The rule: releasing a reservation returns stock and a discount-code slot to
// the tenant, so doing it twice inflates their inventory and they oversell. It
// is only safe when this caller is the one that moved the order out of the
// state that held the reservation. That means a compare-and-set on the order
// row must have succeeded first, in the same function, before the release.

// casQueries are the store calls that atomically claim an order transition.
// Both return rows-affected; zero means another writer got there first.
var casQueries = map[string]bool{
	"UpdateOrderStatusFrom": true,
	"ClaimOrderForRefund":   true,
}

const releaseFunc = "releaseReservations"

// calleeName returns the called function's identifier, ignoring the receiver.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func isBlank(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "_"
}

type siteRef struct {
	fn  string
	pos token.Position
}

// packageFiles returns the non-test Go files of this package.
func packageFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(".", n))
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no non-test Go files found; the walk is broken, not the code")
	}
	return out
}

// TestReleaseReservations_EveryCallSiteIsGatedOnACompareAndSet walks the real
// AST of this package and asserts the invariant for every caller it finds.
func TestReleaseReservations_EveryCallSiteIsGatedOnACompareAndSet(t *testing.T) {
	fset := token.NewFileSet()

	var (
		releaseSites []siteRef
		ungated      []siteRef
		discarded    []siteRef
	)

	for _, path := range packageFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			// Compare-and-set calls whose rows-affected result is bound to a
			// real variable. A CAS assigned to `_` is not a guard: the caller
			// cannot have branched on it. A CAS used as a bare statement is not
			// collected here at all, which is the same verdict.
			var guards []token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, rhs := range assign.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok || !casQueries[calleeName(call)] {
						continue
					}
					if len(assign.Lhs) == 0 || isBlank(assign.Lhs[0]) {
						discarded = append(discarded, siteRef{fn.Name.Name, fset.Position(call.Pos())})
						continue
					}
					guards = append(guards, call.Pos())
				}
				return true
			})

			// Every release call in this function.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || calleeName(call) != releaseFunc {
					return true
				}
				ref := siteRef{fn.Name.Name, fset.Position(call.Pos())}
				releaseSites = append(releaseSites, ref)

				gated := false
				for _, g := range guards {
					if g < call.Pos() {
						gated = true
						break
					}
				}
				if !gated {
					ungated = append(ungated, ref)
				}
				return true
			})
		}
	}

	// Anti-vacuity. A walk that silently finds nothing is the "guard that
	// asserts rather than configures" shape the audit named three times: it
	// would pass forever while covering nothing. If releaseReservations is
	// renamed, this fires and someone updates releaseFunc deliberately.
	if len(releaseSites) == 0 {
		t.Fatalf("found no calls to %s; the guard is not covering anything", releaseFunc)
	}
	for _, s := range releaseSites {
		t.Logf("release path: %s at %s", s.fn, s.pos)
	}

	for _, s := range discarded {
		t.Errorf("%s at %s: compare-and-set result assigned to _; "+
			"the caller cannot have checked whether it won, so it is not a guard", s.fn, s.pos)
	}
	for _, s := range ungated {
		t.Errorf("%s at %s calls %s with no preceding compare-and-set on the order row.\n"+
			"  A release returns stock and a discount slot to the tenant. Doing it twice inflates\n"+
			"  their inventory and they oversell. Gate it on UpdateOrderStatusFrom (or\n"+
			"  ClaimOrderForRefund) winning, and skip the release when it affects zero rows.",
			s.fn, s.pos, releaseFunc)
	}
}

// TestOrderStatus_HasNoUnguardedWriteQuery pins the other half: the compare-and-set
// is only load-bearing while there is no sibling query that writes orders.status
// without naming a from-state. Deleting plain UpdateOrderStatus is what makes the
// guard above impossible to route around, so re-adding one must be a deliberate,
// visible act rather than a convenience someone reaches for at 5pm.
func TestOrderStatus_HasNoUnguardedWriteQuery(t *testing.T) {
	q := reflect.TypeOf((*store.Querier)(nil)).Elem()
	for i := 0; i < q.NumMethod(); i++ {
		if name := q.Method(i).Name; name == "UpdateOrderStatus" {
			t.Fatalf("store.Querier exposes %s, an unguarded write to orders.status.\n"+
				"  Every status write must name the state it expects (UpdateOrderStatusFrom).\n"+
				"  If a caller genuinely cannot, give it an explicitly named query so the\n"+
				"  exception is visible in the surface rather than implied by a comment.", name)
		}
	}
}
