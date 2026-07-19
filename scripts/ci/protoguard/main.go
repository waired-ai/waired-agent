// Command protoguard enforces the additive-only rule for the published
// proto module (docs/decisions.md 20260719). Published proto/v* versions
// are immutable and consumers depend on unset fields staying byte-absent
// from canonical JSON, so relative to the last published tag:
//
//   - exported structs, fields, consts, funcs, and types may not be
//     removed;
//   - field types and struct tags may not change, const values may not
//     change, func signatures may not change;
//   - fields ADDED to a previously-published struct must carry a json
//     tag with omitempty (or json:"-"), so the zero value stays off the
//     wire and old signatures keep verifying.
//
// Brand-new types/consts/funcs are always allowed.
//
// Usage: protoguard <old-proto-dir> <new-proto-dir>
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: protoguard <old-proto-dir> <new-proto-dir>")
		os.Exit(2)
	}
	violations, err := run(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "protoguard: %v\n", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Printf("::error::proto additive-only violation: %s\n", v)
		}
		os.Exit(1)
	}
	fmt.Println("protoguard: OK (all published API intact, additions are omitempty)")
}

func run(oldDir, newDir string) ([]string, error) {
	oldAPI, err := collect(oldDir)
	if err != nil {
		return nil, fmt.Errorf("parse old (%s): %w", oldDir, err)
	}
	newAPI, err := collect(newDir)
	if err != nil {
		return nil, fmt.Errorf("parse new (%s): %w", newDir, err)
	}
	var out []string
	for pkg, oldP := range oldAPI {
		newP, ok := newAPI[pkg]
		if !ok {
			out = append(out, fmt.Sprintf("package %s: removed", pkg))
			continue
		}
		out = append(out, comparePkg(pkg, oldP, newP)...)
	}
	return out, nil
}

type pkgAPI struct {
	structs map[string]map[string]fieldInfo
	consts  map[string]string
	funcs   map[string]string
	types   map[string]string // exported non-struct types -> underlying expr
}

type fieldInfo struct {
	typ string
	tag string
}

func comparePkg(pkg string, oldP, newP *pkgAPI) []string {
	var out []string
	for name, oldFields := range oldP.structs {
		newFields, ok := newP.structs[name]
		if !ok {
			out = append(out, fmt.Sprintf("%s.%s: struct removed", pkg, name))
			continue
		}
		for fname, of := range oldFields {
			nf, ok := newFields[fname]
			if !ok {
				out = append(out, fmt.Sprintf("%s.%s.%s: field removed", pkg, name, fname))
				continue
			}
			if nf.typ != of.typ {
				out = append(out, fmt.Sprintf("%s.%s.%s: type changed %q -> %q", pkg, name, fname, of.typ, nf.typ))
			}
			if nf.tag != of.tag {
				out = append(out, fmt.Sprintf("%s.%s.%s: struct tag changed %q -> %q", pkg, name, fname, of.tag, nf.tag))
			}
		}
		for fname, nf := range newFields {
			if _, existed := oldFields[fname]; existed {
				continue
			}
			if !addedFieldSafe(nf.tag) {
				out = append(out, fmt.Sprintf("%s.%s.%s: field added to a published struct without omitempty (or json:\"-\") — the zero value would change canonical JSON for existing payloads", pkg, name, fname))
			}
		}
	}
	for name, oldVal := range oldP.consts {
		newVal, ok := newP.consts[name]
		if !ok {
			out = append(out, fmt.Sprintf("%s.%s: const removed", pkg, name))
			continue
		}
		if newVal != oldVal {
			out = append(out, fmt.Sprintf("%s.%s: const value changed %s -> %s", pkg, name, oldVal, newVal))
		}
	}
	for name, oldSig := range oldP.funcs {
		newSig, ok := newP.funcs[name]
		if !ok {
			out = append(out, fmt.Sprintf("%s.%s: func removed", pkg, name))
			continue
		}
		if newSig != oldSig {
			out = append(out, fmt.Sprintf("%s.%s: signature changed %q -> %q", pkg, name, oldSig, newSig))
		}
	}
	for name, oldT := range oldP.types {
		newT, ok := newP.types[name]
		if !ok {
			if _, becameStruct := newP.structs[name]; becameStruct {
				out = append(out, fmt.Sprintf("%s.%s: type changed to a struct", pkg, name))
			} else {
				out = append(out, fmt.Sprintf("%s.%s: type removed", pkg, name))
			}
			continue
		}
		if newT != oldT {
			out = append(out, fmt.Sprintf("%s.%s: underlying type changed %q -> %q", pkg, name, oldT, newT))
		}
	}
	return out
}

// addedFieldSafe reports whether a field newly added to a published
// struct keeps the wire form of existing payloads: excluded from JSON
// entirely, or omitted at its zero value.
func addedFieldSafe(rawTag string) bool {
	jsonTag := reflect.StructTag(strings.Trim(rawTag, "`")).Get("json")
	if jsonTag == "-" {
		return true
	}
	parts := strings.Split(jsonTag, ",")
	for _, p := range parts[1:] {
		if p == "omitempty" {
			return true
		}
	}
	return false
}

// collect parses every non-test .go file under root and returns the
// exported API per package directory (keyed by slash-separated path
// relative to root; "." for root itself).
func collect(root string) (map[string]*pkgAPI, error) {
	api := map[string]*pkgAPI{}
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
		p, ok := api[pkg]
		if !ok {
			p = &pkgAPI{
				structs: map[string]map[string]fieldInfo{},
				consts:  map[string]string{},
				funcs:   map[string]string{},
				types:   map[string]string{},
			}
			api[pkg] = p
		}
		collectFile(p, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api, nil
}

func collectFile(p *pkgAPI, f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			key := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				key = types.ExprString(d.Recv.List[0].Type) + "." + key
			}
			p.funcs[key] = types.ExprString(d.Type)
		case *ast.GenDecl:
			switch d.Tok {
			case token.CONST:
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if !name.IsExported() {
							continue
						}
						val := ""
						if i < len(vs.Values) {
							val = types.ExprString(vs.Values[i])
						}
						p.consts[name.Name] = val
					}
				}
			case token.TYPE:
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					if !ts.Name.IsExported() {
						continue
					}
					if st, isStruct := ts.Type.(*ast.StructType); isStruct {
						p.structs[ts.Name.Name] = collectFields(st)
					} else {
						p.types[ts.Name.Name] = types.ExprString(ts.Type)
					}
				}
			}
		}
	}
}

func collectFields(st *ast.StructType) map[string]fieldInfo {
	fields := map[string]fieldInfo{}
	for _, field := range st.Fields.List {
		tag := ""
		if field.Tag != nil {
			tag = field.Tag.Value
		}
		info := fieldInfo{typ: types.ExprString(field.Type), tag: tag}
		if len(field.Names) == 0 {
			// Embedded field: identified by its type name.
			fields[types.ExprString(field.Type)] = info
			continue
		}
		for _, name := range field.Names {
			if name.IsExported() {
				fields[name.Name] = info
			}
		}
	}
	return fields
}
