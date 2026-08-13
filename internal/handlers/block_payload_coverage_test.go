package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Coverage guard for every writer of a block payload.
//
// The 2026-08-11 audit found three writers that never ran the guardrail engine:
// the global-block upsert on two surfaces, the translation overlay, and the
// revision restore. They were writers 4, 5 and 6 to be missed. Each miss had
// the same cause: someone added a new way to put tenant-authored JSON into a
// data_json column and no test could tell that the rule had been skipped.
//
// So this test does not hold a hand-written list of writers. It walks the real
// AST of the whole module and finds them, the way
// orders_release_coverage_test.go does for the reservation-release paths. A
// seventh writer added next year is covered on the day it is written, and
// retention/coverage_test.go (a hand-maintained map, blind to a table nobody
// adds) is the counter-example this shape exists to avoid.
//
// The rule: a call to any store method that writes a block payload must be
// preceded, in the same function or closure, by a guardrail call whose result
// the caller actually branched on. The only exemptions are the seeders in
// allowlistedSeeders, which write payloads this codebase authored itself.

// blockPayloadWriters are the store methods that put a tenant-authored payload
// into a rendered column: blocks.data_json, global_blocks.data_json,
// block_locales.data_json. Anything reaching one of these ends up in published
// HTML, so anything reaching one of these is a guardrail boundary.
//
// UpdateBlockOrder is deliberately absent: it moves a row, it does not carry a
// payload.
var blockPayloadWriters = []string{
	"CreateBlock",
	"CreateGlobalBlock",
	"UpdateBlock",
	"UpdateGlobalBlock",
	"UpsertBlockLocale",
}

// guardCalls are the functions that run the guardrail engine over a block
// payload and hand the caller a refuse/allow decision.
//
// Keep this set small. It is deliberately not "anything with Validate in the
// name": every entry is a real choke point, and every entry is asserted below
// to be reachable, so renaming one fails this test loudly instead of quietly
// exempting the callers that used it.
var guardCalls = map[string]bool{
	// internal/handlers: the one helper every writer in this package uses.
	"validateBlockWrite": true,
	// internal/mcp: the tool handlers' param shadows the `agent` package name,
	// so they cannot call the engine directly and route through this instead.
	"guardrailBlockWrite": true,
	// internal/agent: the engine method itself, for callers outside the two
	// wrappers above.
	"ValidateBlock": true,
}

// allowlistedSeeders are the functions allowed to write a block payload without
// a guardrail pass, keyed "<package>.<func>". Each writes content this codebase
// authored, at site-creation time, before any tenant rule can exist.
//
// An entry here is a deliberate, reviewed exemption. Every entry is asserted to
// still match a real writing function, so renaming a seeder fails the test
// rather than leaving a dead exemption behind that a future writer could
// accidentally inherit.
var allowlistedSeeders = map[string]string{
	"agent.SeedDefaultGlobalBlocksWith": "seeds the default header/footer for a brand-new site from data this repo authored; runs before the site has any guardrail rows.",
	"starterkits.seedBlocks":            "installs a starter kit's own block definitions, which ship in this repo and are reviewed here.",
}

// blockWriteSite is one located call.
type blockWriteSite struct {
	pkg  string
	fn   string
	call string
	pos  token.Position
}

func (s blockWriteSite) key() string { return s.pkg + "." + s.fn }

// moduleGoFiles returns every non-test .go file in the module, found by walking
// up from this package to the directory holding go.mod. Walking the module (not
// just this package) is the point: two of the six writers the audit found were
// in internal/handlers, but the seeders are in internal/agent and
// internal/starterkits and the MCP writers are in internal/mcp, so a
// package-scoped walk would have declared victory while missing them.
// moduleRoot resolves the atomicsite module root from this package's
// directory (internal/handlers, two levels down). Shared by moduleGoFiles
// and the SQL-derivation walk below so both look at the same tree.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	return root
}

func moduleGoFiles(t *testing.T) (string, []string) {
	t.Helper()
	root := moduleRoot(t)
	var out []string
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "frontend": true,
		"dist": true, "testdata": true, "vendor": true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	sort.Strings(out)
	// Anti-vacuity on the walk itself. A walk that finds nothing passes every
	// assertion below while covering nothing, which is the "guard that asserts
	// rather than configures" shape the audit named three times.
	if len(out) < 100 {
		t.Fatalf("module walk found only %d Go files under %s; the walk is broken, not the code", len(out), root)
	}
	return root, out
}

// --- SQL-derived writer list -------------------------------------------
//
// blockPayloadWriters above is a hand list: a name every writer must appear
// under, verified by anti-vacuity 1 to still resolve to a call site. But
// "still resolves" only checks names that are ALREADY on the list; a new
// sqlc query that writes a block payload is invisible until someone
// remembers to add it here. That is exactly how writers 4-6 were missed,
// and it is how a seventh (a new store method, not a new caller of an
// existing one) would be missed again.
//
// TestBlockPayloadWriters_MatchesSQLPayloadQueries below closes that gap by
// deriving the writer set straight from internal/db/queries/*.sql instead of
// trusting the list: it flags any query that INSERTs or UPDATEs the
// data_json column on blocks, global_blocks, or block_locales, and fails if
// that set and blockPayloadWriters disagree in either direction.

// blockPayloadTables are the tables whose data_json column is the rendered
// block payload this guard exists to cover. Verified against
// internal/db/schema.sql: blocks, global_blocks and block_locales each carry
// exactly one such column, data_json; style_json on the same rows is
// presentation, not tenant content, and out of scope (confirmed by grepping
// every writer of it, which is the same five methods below). Scoping by
// TABLE, not by query filename, means a payload-writing query landing in any
// .sql file, including a new or misnamed one, is still caught. collections.sql
// also writes a column named data_json, on collection_items, which is a
// different feature (CMS collection entries, not page-builder blocks) and is
// correctly out of scope here because its table isn't in this set.
var blockPayloadTables = map[string]bool{
	"blocks":        true,
	"global_blocks": true,
	"block_locales": true,
}

// blockPayloadColumn is the column blockPayloadTables render as content.
const blockPayloadColumn = "data_json"

// sqlQuery is one sqlc "-- name: X :verb" statement, with its body (the SQL
// text from just after that marker to the next one, or EOF).
type sqlQuery struct {
	name string
	body string
}

var sqlNameLine = regexp.MustCompile(`(?m)^-- name:\s*(\S+)\s*:\w+\s*$`)

// parseSQLQueries splits a sqlc query file into its named statements.
func parseSQLQueries(t *testing.T, path string) []sqlQuery {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(data)
	locs := sqlNameLine.FindAllStringSubmatchIndex(content, -1)
	var out []sqlQuery
	for i, loc := range locs {
		name := content[loc[2]:loc[3]]
		start := loc[1]
		end := len(content)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, sqlQuery{name: name, body: content[start:end]})
	}
	return out
}

// stripSQLComments drops "--" comment lines so a comment that happens to
// mention a table or column name (internal/db/queries/forms.sql does, for
// form_submissions.data_json) can never be mistaken for a statement.
func stripSQLComments(body string) string {
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "--") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

var (
	insertIntoRe  = regexp.MustCompile(`(?is)INSERT\s+INTO\s+(\w+)\s*\(([^)]*)\)`)
	updateTableRe = regexp.MustCompile(`(?is)UPDATE\s+(\w+)\s+SET\b`)
	doUpdateSetRe = regexp.MustCompile(`(?is)DO\s+UPDATE\s+SET\b`)
	whereRe       = regexp.MustCompile(`(?i)\bWHERE\b`)
)

// columnListed reports whether column appears in an INSERT column list.
func columnListed(cols, column string) bool {
	for _, c := range strings.Split(cols, ",") {
		if strings.EqualFold(strings.TrimSpace(c), column) {
			return true
		}
	}
	return false
}

// columnAssigned reports whether column is set in a SET clause. Callers
// trim the clause to end at WHERE first, so a filter that happens to compare
// the column ("WHERE data_json = ?") is never read as a write.
func columnAssigned(clause, column string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(column) + `\s*=`)
	return re.MatchString(clause)
}

func cutAtWhere(s string) string {
	if loc := whereRe.FindStringIndex(s); loc != nil {
		return s[:loc[0]]
	}
	return s
}

// writesPayloadColumn reports the table a query's INSERT or UPDATE targets,
// and whether it writes column on that table. UPDATE ... SET and INSERT ...
// ON CONFLICT DO UPDATE SET are both checked, each trimmed to end at WHERE
// so a read filter is never mistaken for a write. Queries that are neither
// INSERT nor UPDATE (SELECT, DELETE) never match.
func writesPayloadColumn(rawBody, column string) (table string, writes bool) {
	body := stripSQLComments(rawBody)

	if m := insertIntoRe.FindStringSubmatch(body); m != nil {
		table = strings.ToLower(m[1])
		if columnListed(m[2], column) {
			return table, true
		}
		if loc := doUpdateSetRe.FindStringIndex(body); loc != nil {
			clause := cutAtWhere(body[loc[1]:])
			if columnAssigned(clause, column) {
				return table, true
			}
		}
		return table, false
	}

	if m := updateTableRe.FindStringSubmatchIndex(body); m != nil {
		table = strings.ToLower(body[m[2]:m[3]])
		clause := cutAtWhere(body[m[1]:])
		return table, columnAssigned(clause, column)
	}

	return "", false
}

// sqlDerivedBlockPayloadWriters walks every .sql file in
// internal/db/queries (a directory listing, not a fixed filename list, so a
// new or misnamed query file is still walked) and returns the sqlc query
// names that write blockPayloadColumn on a table in blockPayloadTables.
func sqlDerivedBlockPayloadWriters(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, "internal", "db", "queries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read query dir %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	// Anti-vacuity on this walk too, same shape as moduleGoFiles: a directory
	// listing that silently returns nothing would pass every assertion below
	// while covering zero queries.
	if len(files) < 10 {
		t.Fatalf("query dir walk found only %d .sql files under %s; the walk is broken, not the schema", len(files), dir)
	}

	var names []string
	for _, f := range files {
		for _, q := range parseSQLQueries(t, f) {
			table, writes := writesPayloadColumn(q.body, blockPayloadColumn)
			if writes && blockPayloadTables[table] {
				names = append(names, q.name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// TestBlockPayloadWriters_MatchesSQLPayloadQueries is the derivation half of
// the coverage guard: it fails if blockPayloadWriters (the hand list
// TestBlockPayloadWrites_EveryWriterRunsTheGuardrailEngine trusts) and the
// set of sqlc queries that actually write a block payload column disagree in
// either direction. A new payload-writing query left off the hand list fails
// here by name, on the day it is added, instead of shipping unguarded and
// undetected the way writers 4-6 did.
func TestBlockPayloadWriters_MatchesSQLPayloadQueries(t *testing.T) {
	root := moduleRoot(t)
	derived := sqlDerivedBlockPayloadWriters(t, root)

	derivedSet := map[string]bool{}
	for _, n := range derived {
		derivedSet[n] = true
	}
	handSet := map[string]bool{}
	for _, n := range blockPayloadWriters {
		handSet[n] = true
	}

	for _, n := range derived {
		if !handSet[n] {
			t.Errorf("sqlc query %q writes a block payload column (data_json on "+
				"blocks/global_blocks/block_locales) but is missing from "+
				"blockPayloadWriters.\n"+
				"  Add it to the list in block_payload_coverage_test.go, then make "+
				"sure every caller runs the guardrail engine before calling it.", n)
		}
	}
	for _, n := range blockPayloadWriters {
		if !derivedSet[n] {
			t.Errorf("blockPayloadWriters lists %q, but no sqlc query in "+
				"internal/db/queries writes data_json on blocks/global_blocks/"+
				"block_locales under that name.\n"+
				"  The query was renamed, removed, or no longer writes the payload "+
				"column; update blockPayloadWriters to match.", n)
		}
	}
	if t.Failed() {
		t.Logf("SQL-derived block payload writers: %v", derived)
		t.Logf("hand-maintained blockPayloadWriters: %v", blockPayloadWriters)
	}
}

func blockCalleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func isBlankIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "_"
}

// funcUnit is one innermost function scope: a FuncDecl body or a FuncLit body.
// Closures get their own unit, because internal/mcp registers every tool as a
// closure inside one enormous registerTools func. Treating that whole func as a
// single scope would let a guard in the create_block closure vouch for a writer
// in the update_block closure, which is precisely the "the fix landed on one of
// two twins" failure this test exists to catch.
type funcUnit struct {
	pkg  string
	name string
	body *ast.BlockStmt
}

// collectUnits returns every function scope in a file, innermost-first, with
// closures named after their enclosing declared function.
func collectUnits(pkg string, file *ast.File) []funcUnit {
	var units []funcUnit
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		units = append(units, funcUnit{pkg, fn.Name.Name, fn.Body})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok || lit.Body == nil {
				return true
			}
			units = append(units, funcUnit{pkg, fn.Name.Name, lit.Body})
			return true
		})
	}
	return units
}

// directCalls returns the calls made in this scope, excluding anything inside a
// nested function literal (those belong to their own unit).
func directCalls(body *ast.BlockStmt) []*ast.CallExpr {
	var calls []*ast.CallExpr
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if c, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, c)
		}
		return true
	}
	ast.Inspect(body, walk)
	return calls
}

// guardPositions returns the positions of guardrail calls in this scope whose
// result the caller bound to a real variable. A guard called as a bare
// statement, or assigned to `_`, is not a guard: the caller cannot have
// branched on the verdict. `if v, bad := validateBlockWrite(...); bad` is an
// AssignStmt in the if's Init, so it is collected.
func guardPositions(body *ast.BlockStmt) (guards []token.Pos, discarded []token.Pos) {
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok || !guardCalls[blockCalleeName(call)] {
				continue
			}
			if len(assign.Lhs) == 0 || isBlankIdent(assign.Lhs[0]) {
				discarded = append(discarded, call.Pos())
				continue
			}
			guards = append(guards, call.Pos())
		}
		return true
	}
	ast.Inspect(body, walk)
	return guards, discarded
}

// TestBlockPayloadWrites_EveryWriterRunsTheGuardrailEngine is the coverage guard.
func TestBlockPayloadWrites_EveryWriterRunsTheGuardrailEngine(t *testing.T) {
	fset := token.NewFileSet()
	root, paths := moduleGoFiles(t)

	type parsed struct {
		pkg  string
		file *ast.File
	}
	var files []parsed
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, parsed{f.Name.Name, f})
	}

	writerSet := map[string]bool{}
	for _, w := range blockPayloadWriters {
		writerSet[w] = true
	}

	found := map[string][]blockWriteSite{} // writer name -> call sites
	guardsSeen := map[string]int{}         // guard name -> times bound to a variable
	seedersMatched := map[string]bool{}    // allowlist key -> actually wrote something
	var ungated, discardedGuards, exempt []blockWriteSite

	for _, pf := range files {
		for _, u := range collectUnits(pf.pkg, pf.file) {
			guards, discarded := guardPositions(u.body)
			// Count bound guard calls by name for the anti-vacuity assert.
			ast.Inspect(u.body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, rhs := range assign.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok {
						continue
					}
					if name := blockCalleeName(call); guardCalls[name] {
						if len(assign.Lhs) > 0 && !isBlankIdent(assign.Lhs[0]) {
							guardsSeen[name]++
						}
					}
				}
				return true
			})

			for _, call := range directCalls(u.body) {
				name := blockCalleeName(call)
				if !writerSet[name] {
					continue
				}
				site := blockWriteSite{u.pkg, u.name, name, fset.Position(call.Pos())}
				// Skip the sqlc-generated definitions themselves: a method body
				// named CreateBlock does not *call* CreateBlock, so this only
				// trips if a future store helper wraps one.
				if u.pkg == "store" {
					continue
				}
				found[name] = append(found[name], site)

				if reason, ok := allowlistedSeeders[site.key()]; ok {
					seedersMatched[site.key()] = true
					exempt = append(exempt, site)
					_ = reason
					continue
				}

				gated := false
				for _, g := range guards {
					if g < call.Pos() {
						gated = true
						break
					}
				}
				if !gated {
					if len(discarded) > 0 {
						discardedGuards = append(discardedGuards, site)
					}
					ungated = append(ungated, site)
				}
			}
		}
	}

	for _, s := range exempt {
		t.Logf("exempt seeder: %s calls %s at %s", s.key(), s.call, rel(root, s.pos))
	}

	// Anti-vacuity 1: every writer name must resolve to at least one call site.
	// Rename CreateGlobalBlock in the store and this fires, instead of the rule
	// silently covering nothing.
	for _, w := range blockPayloadWriters {
		if len(found[w]) == 0 {
			t.Errorf("no call sites found for block payload writer %q.\n"+
				"  Either the store method was renamed (update blockPayloadWriters) or the\n"+
				"  walk is broken. A rule covering zero call sites passes forever.", w)
			continue
		}
		for _, s := range found[w] {
			t.Logf("%s: %s at %s", w, s.key(), rel(root, s.pos))
		}
	}

	// Anti-vacuity 2: every guard name must be reachable. Rename
	// validateBlockWrite and this fires, rather than every caller in
	// internal/handlers silently becoming "ungated" or, worse, the set being
	// widened to make the test pass again.
	for name := range guardCalls {
		if guardsSeen[name] == 0 {
			t.Errorf("guardrail call %q is never used with its result bound to a variable.\n"+
				"  It was renamed or removed. Update guardCalls deliberately; do not delete\n"+
				"  the entry to make this pass.", name)
		}
	}

	// Anti-vacuity 3: every allowlisted seeder must still be a real writer. A
	// stale exemption is how a renamed function ends up unguarded and unnoticed.
	for key := range allowlistedSeeders {
		if !seedersMatched[key] {
			t.Errorf("allowlisted seeder %q writes no block payload any more.\n"+
				"  Remove the exemption, or fix the key if the function was renamed.", key)
		}
	}

	for _, s := range discardedGuards {
		t.Errorf("%s at %s: a guardrail call in this scope discards its result.\n"+
			"  The caller cannot have refused the write, so it is not a guard.",
			s.key(), rel(root, s.pos))
	}

	for _, s := range ungated {
		t.Errorf("%s at %s calls %s with no preceding guardrail call in the same scope.\n"+
			"  This writes a tenant-authored payload into a column the builder renders, so a\n"+
			"  <script> refused by create_block would be accepted here. Call\n"+
			"  validateBlockWrite (internal/handlers) or guardrailBlockWrite (internal/mcp)\n"+
			"  before the write and refuse on the returned verdict. If this really is a\n"+
			"  first-party seeder, add it to allowlistedSeeders with the reason.",
			s.key(), rel(root, s.pos), s.call)
	}
}

func rel(root string, p token.Position) string {
	if r, err := filepath.Rel(root, p.Filename); err == nil {
		return r + ":" + strconv.Itoa(p.Line)
	}
	return p.String()
}
