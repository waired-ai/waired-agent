// Command protoconsumer fails when an exported field of an exported
// proto struct has no producer in this repository: nothing under cmd/ or
// internal/ ever assigns it outside tests.
//
// Why this exists: PR #146 was merged onto a feature branch and never
// reached main, so `vendor` / `unified_memory` / `usable_vram_mb` kept a
// consumer in the control plane while the agent-side producer vanished
// (#180). Nothing went red. The existing proto-additive-guard cannot see
// this class by construction — it compares API shape, and the additive
// rule REQUIRES `omitempty`, so a field nobody writes is byte-identical
// on the wire to a field that does not exist. proto tests, profiler
// tests and control-plane tests were all green with the field unwritten.
//
// The check is deliberately syntactic and name-based (see collectWrites):
// it answers "does anything in this repo ever write a field by this
// name", not "does anything write THIS field". That is enough for the
// class it targets — a newly published field whose producer never landed
// — and it needs no type checking, no module loading and no dependency
// beyond the standard library, matching scripts/ci/protoguard.
//
// Fields with no producer under cmd/ or internal/ are declared in
// exemptions.go, which is the single source of truth for them, in three
// categories that each carry a DIFFERENT claim about where the value
// comes from. Every claim is verified, not taken on trust: an entry
// naming a field that no longer exists fails, an entry for a field that
// has since gained a writer fails, an entry claiming a proto-internal
// producer fails when nothing under proto/ writes it, and an entry
// claiming an external producer fails when proto/ does write it. A stale
// or miscategorised exemption is a violation, not a silent pass — that
// is what keeps the table honest.
//
// Usage: protoconsumer [proto-dir [src-dir...]]
// Defaults: proto cmd internal
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	protoDir, srcDirs := "proto", []string{"cmd", "internal"}
	if len(os.Args) > 1 {
		protoDir = os.Args[1]
	}
	if len(os.Args) > 2 {
		srcDirs = os.Args[2:]
	}

	exempt, err := repoExemptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "protoconsumer: %v\n", err)
		os.Exit(2)
	}
	violations, stats, err := guard(protoDir, srcDirs, exempt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "protoconsumer: %v\n", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Printf("::error::%s\n", v)
		}
		fmt.Fprint(os.Stderr, help)
		os.Exit(1)
	}
	fmt.Printf("protoconsumer: OK (%d exported proto fields, %d with a producer under cmd/ or internal/, %d declared)\n",
		stats.fields, stats.produced, stats.exempt)
}

const help = `
Every exported field of an exported proto struct needs a producer: some
non-test code under cmd/ or internal/ that assigns it. A field with a
consumer but no producer ships as a silently absent value — that is how
vendor / unified_memory / usable_vram_mb reached the control plane empty
(#180), with proto, agent and control-plane tests all green.

Write the field if the producer was lost — a PR merged onto a non-main
base is how #146 disappeared. Otherwise declare it in
scripts/ci/protoconsumer/exemptions.go, in the category that is TRUE:

  receiveOnly      someone else writes it: the control plane, a relay, a
                   peer, the catalog authoring pipeline.
  producedInProto  proto writes it itself — a constructor, a decoder, or
                   a pure computation. Not every proto package is a wire
                   schema.
  producerPending  this repo owes the writer. Name the issue that lands
                   it, and delete the entry in that PR.

Categories are checked, not trusted: an entry whose field no longer
exists fails, one for a field that has since gained a writer fails, one
claiming a proto-internal producer fails when proto/ does not write it,
and one claiming an external producer fails when proto/ does.
`

type fieldKey struct{ Pkg, Struct, Field string }

func (k fieldKey) String() string { return k.Pkg + "." + k.Struct + "." + k.Field }

// kind is the claim an exemption makes about where the value comes from.
// Each one is checked against the source, so a wrong category fails as
// loudly as a missing entry.
type kind int

const (
	// receiveOnly: written outside this repository entirely — the
	// control plane, a relay, a peer, the catalog authoring pipeline.
	receiveOnlyKind kind = iota
	// producedInProto: written by the proto module's own code. A
	// constructor, a decoder, or a pure computation whose result type
	// lives beside it. Not every proto package is a wire schema.
	producedInProtoKind
	// producerPending: this repo owes the writer and has not landed it.
	producerPendingKind
)

func (k kind) String() string {
	switch k {
	case receiveOnlyKind:
		return "receiveOnly"
	case producedInProtoKind:
		return "producedInProto"
	default:
		return "producerPending"
	}
}

type claim struct {
	Kind   kind
	Reason string
}

type stats struct{ fields, produced, exempt int }

// guard reports every exported proto field with no producer and no
// exemption, plus every exemption that no longer describes reality.
func guard(protoDir string, srcDirs []string, exempt map[fieldKey]claim) ([]string, stats, error) {
	fields, err := collectProtoFields(protoDir)
	if err != nil {
		return nil, stats{}, fmt.Errorf("parse proto (%s): %w", protoDir, err)
	}
	writes := map[string]bool{}
	for _, dir := range srcDirs {
		if err := collectWrites(dir, writes); err != nil {
			return nil, stats{}, fmt.Errorf("parse source (%s): %w", dir, err)
		}
	}
	protoWrites := map[string]bool{}
	if err := collectWrites(protoDir, protoWrites); err != nil {
		return nil, stats{}, fmt.Errorf("parse proto writers (%s): %w", protoDir, err)
	}

	var out []string
	st := stats{fields: len(fields)}
	for _, k := range sortedKeys(fields) {
		c, exempted := exempt[k]
		switch {
		case writes[k.Field]:
			st.produced++
			if exempted {
				out = append(out, fmt.Sprintf(
					"%s: listed as %s but something under %s now writes it — delete the entry",
					k, c.Kind, joinDirs(srcDirs)))
			}
		case exempted:
			st.exempt++
			switch {
			case c.Kind == producedInProtoKind && !protoWrites[k.Field]:
				out = append(out, fmt.Sprintf(
					"%s: listed as producedInProto, but nothing under %s/ writes it either", k, protoDir))
			case c.Kind != producedInProtoKind && protoWrites[k.Field]:
				out = append(out, fmt.Sprintf(
					"%s: listed as %s, but %s/ writes it — move the entry to producedInProto",
					k, c.Kind, protoDir))
			}
		default:
			out = append(out, fmt.Sprintf(
				"%s: no producer — nothing under %s assigns it outside tests", k, joinDirs(srcDirs)))
		}
	}
	// The other direction: an exemption for a field that no longer exists
	// would sit in the table forever, quietly covering nothing.
	for _, k := range sortedKeys(exempt) {
		if _, ok := fields[k]; !ok {
			out = append(out, fmt.Sprintf(
				"%s: listed as %s but no such exported proto field exists — delete the entry",
				k, exempt[k].Kind))
		}
	}
	return out, st, nil
}

func joinDirs(dirs []string) string { return strings.Join(dirs, "/, ") + "/" }

func sortedKeys[V any](m map[fieldKey]V) []fieldKey {
	keys := make([]fieldKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

// collectProtoFields returns every exported field of every exported
// struct under root, keyed by (package dir relative to root, struct,
// field). Embedded fields are skipped: they carry no value of their own.
func collectProtoFields(root string) (map[fieldKey]bool, error) {
	fields := map[fieldKey]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, isStruct := ts.Type.(*ast.StructType)
				if !isStruct || !ts.Name.IsExported() {
					continue
				}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						if name.IsExported() {
							fields[fieldKey{pkg, ts.Name.Name, name.Name}] = true
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// collectWrites adds the name of every field written by non-test code
// under root: keyed composite-literal elements (`Vendor: v`), assignment
// targets (`x.Vendor = v`, `x.N += 1`), `x.N++`, and fields whose
// address or a slice/element of which is taken (`&x.Key`, `x.Key[:]`,
// `x.Key[i]`) — the usual way to fill an array field indirectly, e.g.
// `copy(h.SrcNodeKey[:], raw)`. Counting an alias as a write is the
// permissive direction on purpose: a scalar field, which is what this
// guard is really watching, is never indexed or sliced.
//
// Name-based on purpose: resolving a selector to its struct would need
// full type checking, which would pull golang.org/x/tools into a repo
// whose CI deliberately runs guards on the standard library alone. The
// cost is a false negative when an unrelated struct happens to have a
// field of the same name; the class this guards against — a brand-new
// proto field whose producer never landed — has no such twin.
func collectWrites(root string, into map[string]bool) error {
	fset := token.NewFileSet()
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				for _, elt := range v.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.IsExported() {
						into[id.Name] = true
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range v.Lhs {
					noteSelector(lhs, into)
				}
			case *ast.IncDecStmt:
				noteSelector(v.X, into)
			case *ast.UnaryExpr:
				if v.Op == token.AND {
					noteSelector(v.X, into)
				}
			case *ast.IndexExpr:
				noteSelector(v.X, into)
			case *ast.SliceExpr:
				noteSelector(v.X, into)
			}
			return true
		})
		return nil
	})
}

func noteSelector(e ast.Expr, into map[string]bool) {
	if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.IsExported() {
		into[sel.Sel.Name] = true
	}
}
