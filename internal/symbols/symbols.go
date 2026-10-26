// Package symbols names SSA functions and Go types consistently across the
// analysis and rendering stages.
package symbols

import (
	"fmt"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// PackageInit is the ssa.Function.Synthetic value of package initializers.
const PackageInit = "package initializer"

// Origin returns the generic origin of an instantiated function, or fn itself.
func Origin(fn *ssa.Function) *ssa.Function {
	if fn == nil {
		return nil
	}
	if o := fn.Origin(); o != nil {
		return o
	}
	return fn
}

// Package returns the package that declares fn. Anonymous functions belong to
// their enclosing function's package; wrappers fall back on their receiver.
func Package(fn *ssa.Function) *types.Package {
	fn = Origin(fn)
	for f := fn; f != nil; f = f.Parent() {
		if f.Pkg != nil {
			return f.Pkg.Pkg
		}
		if obj := f.Object(); obj != nil && obj.Pkg() != nil {
			return obj.Pkg()
		}
	}
	if fn == nil {
		return nil
	}
	if recv := fn.Signature.Recv(); recv != nil {
		if n := Named(recv.Type()); n != nil {
			return n.Obj().Pkg()
		}
	}
	if len(fn.FreeVars) > 0 {
		if n := Named(fn.FreeVars[0].Type()); n != nil {
			return n.Obj().Pkg()
		}
	}
	return nil
}

// PkgPath returns the import path of the package declaring fn, or "".
func PkgPath(fn *ssa.Function) string {
	if p := Package(fn); p != nil {
		return p.Path()
	}
	return ""
}

// Named returns the named type behind t (dereferencing one pointer), or nil.
func Named(t types.Type) *types.Named {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, _ := t.(*types.Named)
	return n
}

// TypeName returns the bare name of the named type behind t, or "".
func TypeName(t types.Type) string {
	if n := Named(t); n != nil {
		return n.Obj().Name()
	}
	return ""
}

// TypePkg returns the import path of the package declaring t's named type.
func TypePkg(t types.Type) string {
	if n := Named(t); n != nil && n.Obj().Pkg() != nil {
		return n.Obj().Pkg().Path()
	}
	return ""
}

// TypeLabel renders t qualified by package *name*, e.g. "*orders.Service".
func TypeLabel(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string { return p.Name() })
}

// IsInterface reports whether t's underlying type is an interface.
func IsInterface(t types.Type) bool {
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

// MethodLabel renders a method as "(*pkg.T).M" or "pkg.T.M".
func MethodLabel(recv types.Type, name string) string {
	if _, ok := types.Unalias(recv).(*types.Pointer); ok {
		return fmt.Sprintf("(%s).%s", TypeLabel(recv), name)
	}
	return TypeLabel(recv) + "." + name
}

// RecvName returns the receiver type name of a method, or "" for functions.
func RecvName(fn *ssa.Function) string {
	fn = Origin(fn)
	if fn == nil {
		return ""
	}
	if recv := fn.Signature.Recv(); recv != nil {
		return TypeName(recv.Type())
	}
	return ""
}

// FuncLabel renders fn as "pkg.Func" or "(*pkg.T).Method".
func FuncLabel(fn *ssa.Function) string {
	fn = Origin(fn)
	if fn == nil {
		return ""
	}
	if recv := fn.Signature.Recv(); recv != nil {
		return MethodLabel(recv.Type(), fn.Name())
	}
	if p := Package(fn); p != nil {
		return p.Name() + "." + fn.Name()
	}
	return fn.Name()
}

// Called names what a call site calls, for lists of the symbols code uses:
// the static callee (its generic origin; a synthetic wrapper, such as a
// promoted method or a bound method value, names the method it stands for), or
// the interface method an invoke calls. ok is false for calls of func values,
// builtins, closures and package initializers, and for unexported names: only
// a package's exported API counts.
func Called(site ssa.CallInstruction) (pkg, label string, ok bool) {
	c := site.Common()
	if c.IsInvoke() {
		t := c.Value.Type()
		pkg = TypePkg(t)
		return pkg, MethodLabel(t, c.Method.Name()), pkg != "" && token.IsExported(c.Method.Name())
	}
	fn := Origin(c.StaticCallee())
	if fn == nil {
		return "", "", false
	}
	if fn.Synthetic != "" {
		obj, isFunc := fn.Object().(*types.Func)
		if !isFunc || obj.Pkg() == nil {
			return "", "", false
		}
		if sig, _ := obj.Type().(*types.Signature); sig != nil && sig.Recv() != nil {
			label = MethodLabel(sig.Recv().Type(), obj.Name())
		} else {
			label = obj.Pkg().Name() + "." + obj.Name()
		}
		return obj.Pkg().Path(), label, token.IsExported(obj.Name())
	}
	pkg = PkgPath(fn)
	return pkg, FuncLabel(fn), pkg != "" && fn.Parent() == nil && token.IsExported(fn.Name())
}

// Short strips the module prefix from a package path.
func Short(pkg, module string) string {
	if pkg == module {
		return path.Base(pkg)
	}
	if module != "" && strings.HasPrefix(pkg, module+"/") {
		return strings.TrimPrefix(pkg, module+"/")
	}
	return pkg
}

// Pos renders pos as "relative/file.go:line".
func Pos(fset *token.FileSet, pos token.Pos, root string) string {
	if fset == nil || !pos.IsValid() {
		return ""
	}
	p := fset.Position(pos)
	file := filepath.ToSlash(p.Filename)
	if rel, err := filepath.Rel(root, p.Filename); root != "" && err == nil && !strings.HasPrefix(rel, "..") {
		file = filepath.ToSlash(rel)
	} else if _, inCache, ok := strings.Cut(file, "/pkg/mod/"); ok {
		// A dependency in the module cache is named by its module path and
		// version (example.com/x-sdk-go@v1.2.0/client.go), not by a path on
		// this machine.
		file = inCache
	}
	return fmt.Sprintf("%s:%d", file, p.Line)
}
