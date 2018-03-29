// Package goose provides compile-time dependency injection logic as a
// Go library.
package goose

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/types/typeutil"
)

// Generate performs dependency injection for a single package,
// returning the gofmt'd Go source code.
func Generate(bctx *build.Context, wd string, pkg string) ([]byte, error) {
	// TODO(light): allow errors
	// TODO(light): stop errors from printing to stderr
	conf := &loader.Config{
		Build:      new(build.Context),
		ParserMode: parser.ParseComments,
		Cwd:        wd,
	}
	*conf.Build = *bctx
	n := len(conf.Build.BuildTags)
	conf.Build.BuildTags = append(conf.Build.BuildTags[:n:n], "gooseinject")
	conf.Import(pkg)
	prog, err := conf.Load()
	if err != nil {
		return nil, fmt.Errorf("load: %v", err)
	}
	if len(prog.InitialPackages()) != 1 {
		// This is more of a violated precondition than anything else.
		return nil, fmt.Errorf("load: got %d packages", len(prog.InitialPackages()))
	}
	pkgInfo := prog.InitialPackages()[0]
	g := newGen(pkgInfo.Pkg.Path())
	mc := newProviderSetCache(prog)
	var directives []directive
	for _, f := range pkgInfo.Files {
		if !isInjectFile(f) {
			continue
		}
		fileScope := pkgInfo.Scopes[f]
		cmap := ast.NewCommentMap(prog.Fset, f, f.Comments)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			directives = directives[:0]
			for _, c := range cmap[fn] {
				directives = extractDirectives(directives, c)
			}
			sets := make([]providerSetRef, 0, len(directives))
			for _, d := range directives {
				if d.kind != "use" {
					return nil, fmt.Errorf("%v: cannot use %s directive on inject function", prog.Fset.Position(d.pos), d.kind)
				}
				ref, err := parseProviderSetRef(d.line, fileScope, g.currPackage, d.pos)
				if err != nil {
					return nil, fmt.Errorf("%v: %v", prog.Fset.Position(d.pos), err)
				}
				sets = append(sets, ref)
			}
			sig := pkgInfo.ObjectOf(fn.Name).Type().(*types.Signature)
			if err := g.inject(mc, fn.Name.Name, sig, sets); err != nil {
				return nil, fmt.Errorf("%v: %v", prog.Fset.Position(fn.Pos()), err)
			}
		}
	}
	goSrc := g.frame(pkgInfo.Pkg.Name())
	fmtSrc, err := format.Source(goSrc)
	if err != nil {
		// This is likely a bug from a poorly generated source file.
		// Return an error and the unformatted source.
		return goSrc, err
	}
	return fmtSrc, nil
}

// gen is the generator state.
type gen struct {
	currPackage string
	buf         bytes.Buffer
	imports     map[string]string
	n           int
}

func newGen(pkg string) *gen {
	return &gen{
		currPackage: pkg,
		imports:     make(map[string]string),
	}
}

// frame bakes the built up source body into an unformatted Go source file.
func (g *gen) frame(pkgName string) []byte {
	if g.buf.Len() == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("// Code generated by goose. DO NOT EDIT.\n\n//+build !gooseinject\n\npackage ")
	buf.WriteString(pkgName)
	buf.WriteString("\n\n")
	if len(g.imports) > 0 {
		buf.WriteString("import (\n")
		imps := make([]string, 0, len(g.imports))
		for path := range g.imports {
			imps = append(imps, path)
		}
		sort.Strings(imps)
		for _, path := range imps {
			fmt.Fprintf(&buf, "\t%s %q\n", g.imports[path], path)
		}
		buf.WriteString(")\n\n")
	}
	buf.Write(g.buf.Bytes())
	return buf.Bytes()
}

// inject emits the code for an injector.
func (g *gen) inject(mc *providerSetCache, name string, sig *types.Signature, sets []providerSetRef) error {
	results := sig.Results()
	returnsErr := false
	switch results.Len() {
	case 0:
		return fmt.Errorf("inject %s: no return values", name)
	case 1:
		// nothing special
	case 2:
		if t := results.At(1).Type(); !types.Identical(t, errorType) {
			return fmt.Errorf("inject %s: second return type is %s; must be error", name, types.TypeString(t, nil))
		}
		returnsErr = true
	default:
		return fmt.Errorf("inject %s: too many return values", name)
	}
	outType := results.At(0).Type()
	params := sig.Params()
	given := make([]types.Type, params.Len())
	for i := 0; i < params.Len(); i++ {
		given[i] = params.At(i).Type()
	}
	calls, err := solve(mc, outType, given, sets)
	if err != nil {
		return err
	}
	for i := range calls {
		if calls[i].hasErr && !returnsErr {
			return fmt.Errorf("inject %s: provider for %s returns error but injection not allowed to fail", name, types.TypeString(calls[i].out, nil))
		}
	}
	g.p("func %s(", name)
	for i := 0; i < params.Len(); i++ {
		if i > 0 {
			g.p(", ")
		}
		pi := params.At(i)
		g.p("%s %s", pi.Name(), types.TypeString(pi.Type(), g.qualifyPkg))
	}
	if returnsErr {
		g.p(") (%s, error) {\n", types.TypeString(outType, g.qualifyPkg))
	} else {
		g.p(") %s {\n", types.TypeString(outType, g.qualifyPkg))
	}
	zv := zeroValue(outType, g.qualifyPkg)
	for i := range calls {
		c := &calls[i]
		g.p("\tv%d", i)
		if c.hasErr {
			g.p(", err")
		}
		g.p(" := %s(", g.qualifiedID(c.importPath, c.funcName))
		for j, a := range c.args {
			if j > 0 {
				g.p(", ")
			}
			if a < params.Len() {
				g.p("%s", params.At(a).Name())
			} else {
				g.p("v%d", a-params.Len())
			}
		}
		g.p(")\n")
		if c.hasErr {
			g.p("\tif err != nil {\n")
			// TODO(light): give information about failing provider
			g.p("\t\treturn %s, err\n", zv)
			g.p("\t}\n")
		}
	}
	if len(calls) == 0 {
		for i := range given {
			if types.Identical(outType, given[i]) {
				g.p("\treturn %s", params.At(i).Name())
				break
			}
		}
	} else {
		g.p("\treturn v%d", len(calls)-1)
	}
	if returnsErr {
		g.p(", nil")
	}
	g.p("\n}\n")
	return nil
}

func (g *gen) qualifiedID(path, sym string) string {
	name := g.qualifyImport(path)
	if name == "" {
		return sym
	}
	return name + "." + sym
}

func (g *gen) qualifyImport(path string) string {
	if path == g.currPackage {
		return ""
	}
	if name := g.imports[path]; name != "" {
		return name
	}
	name := fmt.Sprintf("pkg%d", g.n)
	g.n++
	g.imports[path] = name
	return name
}

func (g *gen) qualifyPkg(pkg *types.Package) string {
	return g.qualifyImport(pkg.Path())
}

func (g *gen) p(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// A providerSet describes a set of providers.  The zero value is an empty
// providerSet.
type providerSet struct {
	providers []*providerInfo
	imports   []providerSetImport
}

type providerSetImport struct {
	providerSetRef
	pos token.Pos
}

const implicitModuleName = "Module"

// findProviderSets processes a package and extracts the provider sets declared in it.
func findProviderSets(fset *token.FileSet, pkg *types.Package, typeInfo *types.Info, files []*ast.File) (map[string]*providerSet, error) {
	sets := make(map[string]*providerSet)
	var directives []directive
	for _, f := range files {
		fileScope := typeInfo.Scopes[f]
		for _, c := range f.Comments {
			directives = extractDirectives(directives[:0], c)
			for _, d := range directives {
				switch d.kind {
				case "provide", "use":
					// handled later
				case "import":
					if fileScope == nil {
						return nil, fmt.Errorf("%s: no scope found for file (likely a bug)", fset.File(f.Pos()).Name())
					}
					var name, spec string
					if strings.HasPrefix(d.line, `"`) {
						name, spec = implicitModuleName, d.line
					} else if i := strings.IndexByte(d.line, ' '); i != -1 {
						name, spec = d.line[:i], d.line[i+1:]
					} else {
						name, spec = implicitModuleName, d.line
					}
					ref, err := parseProviderSetRef(spec, fileScope, pkg.Path(), d.pos)
					if err != nil {
						return nil, fmt.Errorf("%v: %v", fset.Position(d.pos), err)
					}
					if ref.importPath != pkg.Path() {
						imported := false
						for _, imp := range pkg.Imports() {
							if ref.importPath == imp.Path() {
								imported = true
								break
							}
						}
						if !imported {
							return nil, fmt.Errorf("%v: provider set %s imports %q which is not in the package's imports", fset.Position(d.pos), name, ref.importPath)
						}
					}
					if mod := sets[name]; mod != nil {
						found := false
						for _, other := range mod.imports {
							if ref == other.providerSetRef {
								found = true
								break
							}
						}
						if !found {
							mod.imports = append(mod.imports, providerSetImport{providerSetRef: ref, pos: d.pos})
						}
					} else {
						sets[name] = &providerSet{
							imports: []providerSetImport{{providerSetRef: ref, pos: d.pos}},
						}
					}
				default:
					return nil, fmt.Errorf("%v: unknown directive %s", fset.Position(d.pos), d.kind)
				}
			}
		}
		cmap := ast.NewCommentMap(fset, f, f.Comments)
		for _, decl := range f.Decls {
			directives = directives[:0]
			for _, cg := range cmap[decl] {
				directives = extractDirectives(directives, cg)
			}
			fn, isFunction := decl.(*ast.FuncDecl)
			var providerSetName string
			for _, d := range directives {
				if d.kind != "provide" {
					continue
				}
				if providerSetName != "" {
					return nil, fmt.Errorf("%v: multiple provide directives for %s", fset.Position(d.pos), fn.Name.Name)
				}
				if !isFunction {
					return nil, fmt.Errorf("%v: only functions can be marked as providers", fset.Position(d.pos))
				}
				if d.line == "" {
					providerSetName = implicitModuleName
				} else {
					// TODO(light): validate identifier
					providerSetName = d.line
				}
			}
			if providerSetName == "" {
				continue
			}
			fpos := fn.Pos()
			sig := typeInfo.ObjectOf(fn.Name).Type().(*types.Signature)
			r := sig.Results()
			var hasErr bool
			switch r.Len() {
			case 1:
				hasErr = false
			case 2:
				if t := r.At(1).Type(); !types.Identical(t, errorType) {
					return nil, fmt.Errorf("%v: wrong signature for provider %s: second return type must be error", fset.Position(fpos), fn.Name.Name)
				}
				hasErr = true
			default:
				return nil, fmt.Errorf("%v: wrong signature for provider %s: must have one return value and optional error", fset.Position(fpos), fn.Name.Name)
			}
			out := r.At(0).Type()
			p := sig.Params()
			provider := &providerInfo{
				importPath: pkg.Path(),
				funcName:   fn.Name.Name,
				pos:        fn.Pos(),
				args:       make([]types.Type, p.Len()),
				out:        out,
				hasErr:     hasErr,
			}
			for i := 0; i < p.Len(); i++ {
				provider.args[i] = p.At(i).Type()
				for j := 0; j < i; j++ {
					if types.Identical(provider.args[i], provider.args[j]) {
						return nil, fmt.Errorf("%v: provider has multiple parameters of type %s", fset.Position(fpos), types.TypeString(provider.args[j], nil))
					}
				}
			}
			if mod := sets[providerSetName]; mod != nil {
				for _, other := range mod.providers {
					if types.Identical(other.out, provider.out) {
						return nil, fmt.Errorf("%v: provider set %s has multiple providers for %s (previous declaration at %v)", fset.Position(fpos), providerSetName, types.TypeString(provider.out, nil), fset.Position(other.pos))
					}
				}
				mod.providers = append(mod.providers, provider)
			} else {
				sets[providerSetName] = &providerSet{
					providers: []*providerInfo{provider},
				}
			}
		}
	}
	return sets, nil
}

// providerSetCache is a lazily evaluated index of provider sets.
type providerSetCache struct {
	sets map[string]map[string]*providerSet
	fset *token.FileSet
	prog *loader.Program
}

func newProviderSetCache(prog *loader.Program) *providerSetCache {
	return &providerSetCache{
		fset: prog.Fset,
		prog: prog,
	}
}

func (mc *providerSetCache) get(ref providerSetRef) (*providerSet, error) {
	if mods, cached := mc.sets[ref.importPath]; cached {
		mod := mods[ref.name]
		if mod == nil {
			return nil, fmt.Errorf("no such provider set %s in package %q", ref.name, ref.importPath)
		}
		return mod, nil
	}
	if mc.sets == nil {
		mc.sets = make(map[string]map[string]*providerSet)
	}
	pkg, info, files, err := mc.getpkg(ref.importPath)
	if err != nil {
		mc.sets[ref.importPath] = nil
		return nil, fmt.Errorf("analyze package: %v", err)
	}
	mods, err := findProviderSets(mc.fset, pkg, info, files)
	if err != nil {
		mc.sets[ref.importPath] = nil
		return nil, err
	}
	mc.sets[ref.importPath] = mods
	mod := mods[ref.name]
	if mod == nil {
		return nil, fmt.Errorf("no such provider set %s in package %q", ref.name, ref.importPath)
	}
	return mod, nil
}

func (mc *providerSetCache) getpkg(path string) (*types.Package, *types.Info, []*ast.File, error) {
	// TODO(light): allow other implementations for testing

	pkg := mc.prog.Package(path)
	if pkg == nil {
		return nil, nil, nil, fmt.Errorf("package %q not found", path)
	}
	return pkg.Pkg, &pkg.Info, pkg.Files, nil
}

// solve finds the sequence of calls required to produce an output type
// with an optional set of provided inputs.
func solve(mc *providerSetCache, out types.Type, given []types.Type, sets []providerSetRef) ([]call, error) {
	for i, g := range given {
		for _, h := range given[:i] {
			if types.Identical(g, h) {
				return nil, fmt.Errorf("multiple inputs of the same type %s", types.TypeString(g, nil))
			}
		}
	}
	providers, err := buildProviderMap(mc, sets)
	if err != nil {
		return nil, err
	}

	// Start building the mapping of type to local variable of the given type.
	// The first len(given) local variables are the given types.
	index := new(typeutil.Map)
	for i, g := range given {
		if p := providers.At(g); p != nil {
			pp := p.(*providerInfo)
			return nil, fmt.Errorf("input of %s conflicts with provider %s at %s", types.TypeString(g, nil), pp.funcName, mc.fset.Position(pp.pos))
		}
		index.Set(g, i)
	}

	// Topological sort of the directed graph defined by the providers
	// using a depth-first search. The graph may contain cycles, which
	// should trigger an error.
	var calls []call
	var visit func(trail []types.Type) error
	visit = func(trail []types.Type) error {
		typ := trail[len(trail)-1]
		if index.At(typ) != nil {
			return nil
		}
		for _, t := range trail[:len(trail)-1] {
			if types.Identical(typ, t) {
				// TODO(light): describe cycle
				return fmt.Errorf("cycle for %s", types.TypeString(typ, nil))
			}
		}

		p, _ := providers.At(typ).(*providerInfo)
		if p == nil {
			if len(trail) == 1 {
				return fmt.Errorf("no provider found for %s (output of injector)", types.TypeString(typ, nil))
			}
			// TODO(light): give name of provider
			return fmt.Errorf("no provider found for %s (required by provider of %s)", types.TypeString(typ, nil), types.TypeString(trail[len(trail)-2], nil))
		}
		for _, a := range p.args {
			// TODO(light): this will discard grown trail arrays.
			if err := visit(append(trail, a)); err != nil {
				return err
			}
		}
		args := make([]int, len(p.args))
		for i := range p.args {
			args[i] = index.At(p.args[i]).(int)
		}
		index.Set(typ, len(given)+len(calls))
		calls = append(calls, call{
			importPath: p.importPath,
			funcName:   p.funcName,
			args:       args,
			out:        typ,
			hasErr:     p.hasErr,
		})
		return nil
	}
	if err := visit([]types.Type{out}); err != nil {
		return nil, err
	}
	return calls, nil
}

func buildProviderMap(mc *providerSetCache, sets []providerSetRef) (*typeutil.Map, error) {
	type nextEnt struct {
		to providerSetRef

		from providerSetRef
		pos  token.Pos
	}

	pm := new(typeutil.Map) // to *providerInfo
	visited := make(map[providerSetRef]struct{})
	var next []nextEnt
	for _, ref := range sets {
		next = append(next, nextEnt{to: ref})
	}
	for len(next) > 0 {
		curr := next[0]
		copy(next, next[1:])
		next = next[:len(next)-1]
		if _, skip := visited[curr.to]; skip {
			continue
		}
		visited[curr.to] = struct{}{}
		mod, err := mc.get(curr.to)
		if err != nil {
			if !curr.pos.IsValid() {
				return nil, err
			}
			return nil, fmt.Errorf("%v: %v", mc.fset.Position(curr.pos), err)
		}
		for _, p := range mod.providers {
			if prev := pm.At(p.out); prev != nil {
				pos := mc.fset.Position(p.pos)
				typ := types.TypeString(p.out, nil)
				prevPos := mc.fset.Position(prev.(*providerInfo).pos)
				if curr.from.importPath != "" {
					return nil, fmt.Errorf("%v: multiple bindings for %s (added by injector, previous binding at %v)", pos, typ, prevPos)
				}
				return nil, fmt.Errorf("%v: multiple bindings for %s (imported by %v, previous binding at %v)", pos, typ, curr.from, prevPos)
			}
			pm.Set(p.out, p)
		}
		for _, imp := range mod.imports {
			next = append(next, nextEnt{to: imp.providerSetRef, from: curr.to, pos: imp.pos})
		}
	}
	return pm, nil
}

// A call represents a step of an injector function.
type call struct {
	// importPath and funcName identify the provider function to call.
	importPath string
	funcName   string

	// args is a list of arguments to call the provider with.  Each element is either:
	// a) one of the givens (args[i] < len(given)) or
	// b) the result of a previous provider call (args[i] >= len(given)).
	args []int

	// out is the type produced by this provider call.
	out types.Type

	// hasErr is true if the provider call returns an error.
	hasErr bool
}

// providerInfo records the signature of a provider function.
type providerInfo struct {
	importPath string
	funcName   string
	pos        token.Pos
	args       []types.Type
	out        types.Type
	hasErr     bool
}

// A providerSetRef is a parsed reference to a collection of providers.
type providerSetRef struct {
	importPath string
	name       string
}

func parseProviderSetRef(ref string, s *types.Scope, pkg string, pos token.Pos) (providerSetRef, error) {
	// TODO(light): verify that provider set name is an identifier before returning

	i := strings.LastIndexByte(ref, '.')
	if i == -1 {
		return providerSetRef{importPath: pkg, name: ref}, nil
	}
	imp, name := ref[:i], ref[i+1:]
	if strings.HasPrefix(imp, `"`) {
		path, err := strconv.Unquote(imp)
		if err != nil {
			return providerSetRef{}, fmt.Errorf("parse provider set reference %q: bad import path", ref)
		}
		return providerSetRef{importPath: path, name: name}, nil
	}
	_, obj := s.LookupParent(imp, pos)
	if obj == nil {
		return providerSetRef{}, fmt.Errorf("parse provider set reference %q: unknown identifier %s", ref, imp)
	}
	pn, ok := obj.(*types.PkgName)
	if !ok {
		return providerSetRef{}, fmt.Errorf("parse provider set reference %q: %s does not name a package", ref, imp)
	}
	return providerSetRef{importPath: pn.Imported().Path(), name: name}, nil
}

func (ref providerSetRef) String() string {
	return strconv.Quote(ref.importPath) + "." + ref.name
}

type directive struct {
	pos  token.Pos
	kind string
	line string
}

func extractDirectives(d []directive, cg *ast.CommentGroup) []directive {
	const prefix = "goose:"
	text := cg.Text()
	for len(text) > 0 {
		text = strings.TrimLeft(text, " \t\r\n")
		if !strings.HasPrefix(text, prefix) {
			break
		}
		line := text[len(prefix):]
		if i := strings.IndexByte(line, '\n'); i != -1 {
			line, text = line[:i], line[i+1:]
		} else {
			text = ""
		}
		if i := strings.IndexByte(line, ' '); i != -1 {
			d = append(d, directive{
				kind: line[:i],
				line: strings.TrimSpace(line[i+1:]),
				pos:  cg.Pos(), // TODO(light): more precise position
			})
		} else {
			d = append(d, directive{
				kind: line,
				pos:  cg.Pos(), // TODO(light): more precise position
			})
		}
	}
	return d
}

// isInjectFile reports whether a given file is an injection template.
func isInjectFile(f *ast.File) bool {
	// TODO(light): better determination
	for _, cg := range f.Comments {
		text := cg.Text()
		if strings.HasPrefix(text, "+build") && strings.Contains(text, "gooseinject") {
			return true
		}
	}
	return false
}

// zeroValue returns the shortest expression that evaluates to the zero
// value for the given type.
func zeroValue(t types.Type, qf types.Qualifier) string {
	switch u := t.Underlying().(type) {
	case *types.Array, *types.Struct:
		return types.TypeString(t, qf) + "{}"
	case *types.Basic:
		info := u.Info()
		switch {
		case info&types.IsBoolean != 0:
			return "false"
		case info&(types.IsInteger|types.IsFloat|types.IsComplex) != 0:
			return "0"
		case info&types.IsString != 0:
			return `""`
		default:
			panic("unreachable")
		}
	case *types.Chan, *types.Interface, *types.Map, *types.Pointer, *types.Signature, *types.Slice:
		return "nil"
	default:
		panic("unreachable")
	}
}

var errorType = types.Universe.Lookup("error").Type()