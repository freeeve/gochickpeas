// The Go source behind each emitted "gochickpeas (go)" cell (task 027):
// the native_*.go and ga_algos.go files are embedded at build time and
// sliced by go/parser positions, so the emitted text and file:line refs
// are exactly the code the swept commit compiled. NativeKernelSources
// maps the registerNative table to function spans and cross-checks the
// live registry; GAKernelSources maps the six Graphalytics algorithms.

package ldbc

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

//go:embed native_*.go ga_algos.go
var kernelSrcFS embed.FS

// KernelSource is one kernel function's sliced source.
type KernelSource struct {
	Family string
	Query  string
	Func   string
	SrcRef string // repo-relative file:line of the func declaration
	Source string // doc comment + full function text
}

// gaKernelFuncs maps each Graphalytics query to its algorithm function
// (the run table in cmd/gabench binds the same six).
var gaKernelFuncs = []struct{ query, fn string }{
	{"BFS", "GABFS"},
	{"PR", "GAPageRank"},
	{"WCC", "GAWCC"},
	{"CDLP", "GACDLPSeeded"},
	{"LCC", "GALCC"},
	{"SSSP", "GASSSP"},
}

// NativeKernelSources returns the source slice for every registerNative
// registration, erroring if extraction disagrees with the live registry
// so a kernel the viz shows a timing for can never silently lack code.
func NativeKernelSources() ([]KernelSource, error) {
	idx, err := indexKernelSources()
	if err != nil {
		return nil, err
	}
	out := make([]KernelSource, 0, len(idx.regs))
	for _, r := range idx.regs {
		if _, ok := NativeKernelFor(r.family, r.query); !ok {
			return nil, fmt.Errorf("extracted %s/%s is not in the kernel registry", r.family, r.query)
		}
		ks, err := idx.slice(r.family, r.query, r.fn)
		if err != nil {
			return nil, err
		}
		out = append(out, ks)
	}
	if len(out) != NativeKernelCount() {
		return nil, fmt.Errorf("extracted %d kernel sources, registry has %d", len(out), NativeKernelCount())
	}
	return out, nil
}

// GAKernelSources returns the source slice for the six Graphalytics
// algorithm kernels (family "GA").
func GAKernelSources() ([]KernelSource, error) {
	idx, err := indexKernelSources()
	if err != nil {
		return nil, err
	}
	out := make([]KernelSource, 0, len(gaKernelFuncs))
	for _, e := range gaKernelFuncs {
		ks, err := idx.slice("GA", e.query, e.fn)
		if err != nil {
			return nil, err
		}
		out = append(out, ks)
	}
	return out, nil
}

// srcIndex holds the parsed embedded sources: raw bytes per file,
// top-level function declarations, and the registerNative registrations
// found in init functions.
type srcIndex struct {
	fset  *token.FileSet
	files map[string][]byte
	funcs map[string]srcFunc
	regs  []srcReg
}

type srcFunc struct {
	file string
	decl *ast.FuncDecl
}

type srcReg struct{ family, query, fn string }

// indexKernelSources parses every embedded file (test files excluded).
func indexKernelSources() (*srcIndex, error) {
	idx := &srcIndex{fset: token.NewFileSet(), files: map[string][]byte{}, funcs: map[string]srcFunc{}}
	entries, err := kernelSrcFS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := kernelSrcFS.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		idx.files[e.Name()] = src
		file, err := parser.ParseFile(idx.fset, e.Name(), src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing embedded %s: %w", e.Name(), err)
		}
		if err := idx.indexFile(e.Name(), file); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

// indexFile records one file's top-level functions and registrations.
func (idx *srcIndex) indexFile(name string, file *ast.File) error {
	var bad error
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil {
			continue
		}
		if fd.Name.Name != "init" {
			idx.funcs[fd.Name.Name] = srcFunc{file: name, decl: fd}
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "registerNative" || len(call.Args) != 3 {
				return true
			}
			family, okF := stringLit(call.Args[0])
			query, okQ := stringLit(call.Args[1])
			fn := kernelFuncName(call.Args[2])
			if !okF || !okQ || fn == "" {
				bad = fmt.Errorf("%s: unresolvable registerNative call at %s", name, idx.fset.Position(call.Pos()))
				return true
			}
			idx.regs = append(idx.regs, srcReg{family: family, query: query, fn: fn})
			return true
		})
	}
	return bad
}

// slice cuts one function's text (doc comment included) out of its file.
func (idx *srcIndex) slice(family, query, fn string) (KernelSource, error) {
	loc, ok := idx.funcs[fn]
	if !ok {
		return KernelSource{}, fmt.Errorf("kernel %s/%s: func %s not found in embedded sources", family, query, fn)
	}
	start := loc.decl.Pos()
	if loc.decl.Doc != nil {
		start = loc.decl.Doc.Pos()
	}
	return KernelSource{
		Family: family,
		Query:  query,
		Func:   fn,
		SrcRef: fmt.Sprintf("internal/ldbc/%s:%d", loc.file, idx.fset.Position(loc.decl.Pos()).Line),
		Source: string(idx.files[loc.file][idx.fset.Position(start).Offset:idx.fset.Position(loc.decl.End()).Offset]),
	}, nil
}

// stringLit unquotes a string-literal argument.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// kernelFuncName unwraps the registered kernel expression: a bare
// function identifier or a simpleKernel(ident) adapter.
func kernelFuncName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "simpleKernel" && len(v.Args) == 1 {
			if arg, ok := v.Args[0].(*ast.Ident); ok {
				return arg.Name
			}
		}
	}
	return ""
}
