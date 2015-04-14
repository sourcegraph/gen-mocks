package main // import "sourcegraph.com/sourcegraph/gen-mocks"

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/imports"
)

var (
	importPath = flag.String("p", "", "package path")
	writeFiles = flag.Bool("w", false, "write over existing files in output directory (default: writes to stdout)")
	outDir     = flag.String("o", "svc", "output directory")
	pkgName    = flag.String("n", "svc", "package name")

	fset = token.NewFileSet()
)

func main() {
	flag.Parse()
	log.SetFlags(0)
	if *importPath == "" {
		log.Fatal("Import path must be set")
	}

	svcPkg, err := parseSvcPkg()
	if err != nil {
		log.Fatal(err)
	}

	svcs, err := readServiceInterfaces(svcPkg)
	if err != nil {
		log.Fatal(err)
	}

	if err := writeMockImplFiles(*outDir, svcPkg.Name, svcs); err != nil {
		log.Fatal(err)
	}
}

func parseSvcPkg() (*ast.Package, error) {
	pkg, err := build.Import(*importPath, "", 0)
	if err != nil {
		log.Fatal(err)
	}
	pkgs, err := parser.ParseDir(fset, pkg.Dir, nil, parser.AllErrors)
	if err != nil {
		log.Fatal(err)
	}
	for _, pkg := range pkgs {
		if pkg.Name == *pkgName {
			return pkg, nil
		}
	}
	return nil, fmt.Errorf("No 'svc' package found in %s.", pkg.Dir)
}

// readServiceInterfaces returns a list of *Service interfaces in
// package svc that should be mocked.
func readServiceInterfaces(pkg *ast.Package) ([]*ast.TypeSpec, error) {
	var ifaces []*ast.TypeSpec
	ast.Walk(visitFn(func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.GenDecl:
			if node.Tok == token.TYPE {
				for _, spec := range node.Specs {
					tspec := spec.(*ast.TypeSpec)
					if _, ok := tspec.Type.(*ast.InterfaceType); !ok {
						continue
					}
					if name := tspec.Name.Name; strings.HasSuffix(name, "Service") {
						ifaces = append(ifaces, tspec)
					}
				}
			}
			return false
		default:
			return true
		}
	}), pkg)
	return ifaces, nil
}

type visitFn func(node ast.Node) (descend bool)

func (v visitFn) Visit(node ast.Node) ast.Visitor {
	descend := v(node)
	if descend {
		return v
	} else {
		return nil
	}
}

func writeMockImplFiles(outDir, pkgName string, svcIfaces []*ast.TypeSpec) error {
	if err := os.MkdirAll(outDir, 0700); err != nil {
		return err
	}
	for _, iface := range svcIfaces {
		var decls []ast.Decl

		// mock method fields on struct
		var methFields []*ast.Field
		for _, methField := range iface.Type.(*ast.InterfaceType).Methods.List {
			if meth, ok := methField.Type.(*ast.FuncType); ok {
				methFields = append(methFields, &ast.Field{
					Names: []*ast.Ident{ast.NewIdent(methField.Names[0].Name + "_")},
					Type:  meth,
				})
			}
		}

		// struct implementation type
		mockTypeName := "Mock" + iface.Name.Name
		implType := &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{&ast.TypeSpec{
			Name: ast.NewIdent(mockTypeName),
			Type: &ast.StructType{Fields: &ast.FieldList{List: methFields}},
		}}}
		decls = append(decls, implType)

		// struct methods
		for _, methField := range iface.Type.(*ast.InterfaceType).Methods.List {
			if meth, ok := methField.Type.(*ast.FuncType); ok {
				decls = append(decls, &ast.FuncDecl{
					Recv: &ast.FieldList{List: []*ast.Field{
						{
							Names: []*ast.Ident{ast.NewIdent("s")},
							Type:  &ast.StarExpr{X: ast.NewIdent(mockTypeName)},
						},
					}},
					Name: ast.NewIdent(methField.Names[0].Name),
					Type: meth,
					Body: &ast.BlockStmt{List: []ast.Stmt{
						&ast.ReturnStmt{Results: []ast.Expr{
							&ast.CallExpr{
								Fun: &ast.SelectorExpr{
									X:   ast.NewIdent("s"),
									Sel: ast.NewIdent(methField.Names[0].Name + "_"),
								},
								Args: fieldListToIdentList(meth.Params),
							},
						}},
					}},
				})
			}
		}

		file := &ast.File{
			Name:  ast.NewIdent(pkgName),
			Decls: decls,
		}
		astutil.AddImport(fset, file, *importPath)
		filename := fset.Position(iface.Pos()).Filename
		filename = filepath.Join(outDir, strings.TrimSuffix(filepath.Base(filename), ".go")+"_mock.go")
		log.Println("#", filename)
		var w io.Writer
		if *writeFiles {
			f, err := os.Create(filename)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		} else {
			w = os.Stdout
		}

		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, file); err != nil {
			return err
		}

		// Always put blank lines between funcs.
		src := bytes.Replace(buf.Bytes(), []byte("}\nfunc"), []byte("}\n\nfunc"), -1)

		var err error
		src, err = imports.Process(filename, src, nil)
		if err != nil {
			return err
		}

		w.Write(src)
	}
	return nil
}

func fieldListToIdentList(fl *ast.FieldList) []ast.Expr {
	var fs []ast.Expr
	for _, f := range fl.List {
		for _, name := range f.Names {
			fs = append(fs, ast.NewIdent(name.Name))
		}
	}
	return fs
}
