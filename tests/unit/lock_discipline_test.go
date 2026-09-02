package unit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// repoRoot is where this test walks from. tests/unit sits two levels down.
const repoRoot = "../.."

// daemonTrees are the trees that ship in the binary.
var daemonTrees = []string{"internal", "packages", "cmd"}

// pulselockPkg is the one package that must name sync's mutexes, because it
// wraps them.
const pulselockPkg = "packages/pulselock"

// TestNoBareSyncMutexInDaemonCode is the static half of END-2339, and it is
// deliberately the only static half.
//
// The original plan was a checker that flagged a locking method calling a
// locking sibling while holding the lock. That was written and run: over
// internal/membership and internal/server it produced 10 candidates, 9 of them
// false positives, and the single true positive was dead code -- while it
// missed Member.RemoveIPs, which was live. Seven of the nine false positives
// were a callee taking a *different* mutex on the same receiver, which is
// legitimate and common: Server has nine mutexes and the eight auxiliary ones
// exist precisely so they can be taken under the main lock. Answering that
// soundly needs transitive mutex-identity resolution through the call graph.
//
// So the static check answers a question that is actually decidable -- is every
// mutex instrumented? -- and delegates "does this deadlock" to runtime, where
// pulselock answers it exactly. See docs/adr/0003-instrumented-mutexes.md.
//
// Tests are exempt: scaffolding that collects output from a goroutine wants a
// plain mutex and has no discipline to protect.
func TestNoBareSyncMutexInDaemonCode(t *testing.T) {
	var offenders []string

	for _, tree := range daemonTrees {
		root := filepath.Join(repoRoot, tree)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if inPulselock(path) {
				return nil
			}

			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}

			// The AST, not a grep. Both this package's own doc comments and
			// pulselock's panic messages contain the string "sync.Mutex", and a
			// grep-based version of this check flagged them.
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "sync" {
					return true
				}
				if sel.Sel.Name != "Mutex" && sel.Sel.Name != "RWMutex" {
					return true
				}
				rel, relErr := filepath.Rel(repoRoot, path)
				if relErr != nil {
					rel = path
				}
				offenders = append(offenders, rel+":"+
					itoa(fset.Position(sel.Pos()).Line)+" sync."+sel.Sel.Name)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	t.Errorf("bare sync mutexes in daemon code (%d):\n\t%s\n\n"+
		"Use pulselock.Mutex or pulselock.RWMutex instead. They embed and wrap the sync\n"+
		"types and match their method sets, so this is a one-word change and every call\n"+
		"site keeps compiling -- and a lock the daemon wedges itself on then says so\n"+
		"instead of hanging in silence. See docs/adr/0003-instrumented-mutexes.md.",
		len(offenders), strings.Join(offenders, "\n\t"))
}

// TestEveryDaemonMutexIsAccountedFor is the other direction, and it exists
// because the check above passes trivially if the walk finds nothing.
//
// A test that scans a tree can silently stop scanning -- a moved directory, a
// changed relative path, a WalkDir that returns early. Then it reports success
// having examined zero files, which is the failure mode of every
// convention-checking test and is indistinguishable from compliance. So assert
// the walk actually found the mutexes we know are there.
func TestEveryDaemonMutexIsAccountedFor(t *testing.T) {
	var found []string

	for _, tree := range daemonTrees {
		root := filepath.Join(repoRoot, tree)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if inPulselock(path) {
				return nil
			}

			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "pulselock" {
					return true
				}
				if sel.Sel.Name == "Mutex" || sel.Sel.Name == "RWMutex" {
					found = append(found, sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	// 20 declarations converted in END-2339: nine on Server plus one function
	// local, one each on vipReconciler, peerBringUpBatcher, QuorumManager,
	// Member, MemberList, HealthChecker and Config, two on IPMonitor, and one
	// function local in packages/network.
	//
	// A lower bound rather than an exact count, so that adding a mutex is not a
	// failing test -- but low enough to catch a walk that has stopped walking.
	const knownAtLeast = 20
	if len(found) < knownAtLeast {
		t.Errorf("found only %d pulselock mutex declarations, expected at least %d — "+
			"the walk is probably not reaching the daemon trees any more, which would "+
			"make TestNoBareSyncMutexInDaemonCode pass without examining anything",
			len(found), knownAtLeast)
	}
}

func inPulselock(path string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	return strings.Contains(cleaned, pulselockPkg)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
