// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scan

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/vuln/internal"
	"golang.org/x/vuln/internal/client"
	"golang.org/x/vuln/internal/govulncheck"
	"golang.org/x/vuln/internal/osv"
	"golang.org/x/vuln/internal/vulncheck"
)

// runSource reports vulnerabilities that affect the analyzed packages.
//
// Vulnerabilities can be called (affecting the package, because a vulnerable
// symbol is actually exercised) or just imported by the package
// (likely having a non-affecting outcome).
func runSource(ctx context.Context, handler govulncheck.Handler, cfg *config, client *client.Client, dir string) error {
	var pkgs []*packages.Package
	graph := vulncheck.NewPackageGraph(cfg.GoVersion)
	pkgConfig := &packages.Config{
		Dir:   dir,
		Tests: cfg.test,
		Env:   cfg.env,
	}
	pkgs, err := graph.LoadPackages(pkgConfig, cfg.tags, cfg.patterns)
	if err != nil {
		// Try to provide a meaningful and actionable error message.
		if !fileExists(filepath.Join(dir, "go.mod")) {
			return fmt.Errorf("govulncheck: %v", errNoGoMod)
		}
		if isGoVersionMismatchError(err) {
			return fmt.Errorf("govulncheck: %v\n\n%v", errGoVersionMismatch, err)
		}
		return fmt.Errorf("govulncheck: loading packages: %w", err)
	}
	if err := handler.Progress(sourceProgressMessage(pkgs)); err != nil {
		return err
	}
	vr, err := vulncheck.Source(ctx, pkgs, &cfg.Config, client, graph)
	if err != nil {
		return err
	}
	callStacks := vulncheck.CallStacks(vr)
	filterCallStacks(callStacks)
	return emitResult(handler, vr, callStacks)
}

func filterCallStacks(callstacks map[*vulncheck.Vuln][]vulncheck.CallStack) {
	type key struct {
		id  string
		pkg string
		mod string
	}
	// Collect all called symbols for a package.
	// Needed for creating unique call stacks.
	vulnsPerPkg := make(map[key][]*vulncheck.Vuln)
	for vv := range callstacks {
		if vv.CallSink != nil {
			k := key{id: vv.OSV.ID, pkg: vv.ImportSink.PkgPath, mod: vv.ImportSink.Module.Path}
			vulnsPerPkg[k] = append(vulnsPerPkg[k], vv)
		}
	}
	for vv, stacks := range callstacks {
		var filtered []vulncheck.CallStack
		if vv.CallSink != nil {
			k := key{id: vv.OSV.ID, pkg: vv.ImportSink.PkgPath, mod: vv.ImportSink.Module.Path}
			vcs := uniqueCallStack(vv, stacks, vulnsPerPkg[k])
			if vcs != nil {
				filtered = []vulncheck.CallStack{vcs}
			}
		}
		callstacks[vv] = filtered
	}
}

func emitResult(handler govulncheck.Handler, vr *vulncheck.Result, callstacks map[*vulncheck.Vuln][]vulncheck.CallStack) error {
	osvs := map[string]*osv.Entry{}
	// first deal with all the affected vulnerabilities
	emitted := map[string]bool{}
	seen := map[string]bool{}
	for _, vv := range vr.Vulns {
		osvs[vv.OSV.ID] = vv.OSV
		fixed := fixedVersion(vv.ImportSink.Module.Path, vv.OSV.Affected)
		stacks := callstacks[vv]
		for _, stack := range stacks {
			emitted[vv.OSV.ID] = true
			emitFinding(handler, osvs, seen, &govulncheck.Finding{
				OSV:          vv.OSV.ID,
				FixedVersion: fixed,
				Trace:        tracefromEntries(stack),
			})
		}
	}
	for _, vv := range vr.Vulns {
		if emitted[vv.OSV.ID] {
			continue
		}
		stacks := callstacks[vv]
		if len(stacks) != 0 {
			continue
		}
		emitted[vv.OSV.ID] = true
		emitFinding(handler, osvs, seen, &govulncheck.Finding{
			OSV:          vv.OSV.ID,
			FixedVersion: fixedVersion(vv.ImportSink.Module.Path, vv.OSV.Affected),
			Trace:        []*govulncheck.Frame{frameFromPackage(vv.ImportSink)},
		})
	}
	return nil
}

func emitFinding(handler govulncheck.Handler, osvs map[string]*osv.Entry, seen map[string]bool, finding *govulncheck.Finding) error {
	if !seen[finding.OSV] {
		seen[finding.OSV] = true
		if err := handler.OSV(osvs[finding.OSV]); err != nil {
			return err
		}
	}
	return handler.Finding(finding)
}

// tracefromEntries creates a sequence of
// frames from vcs. Position of a Frame is the
// call position of the corresponding stack entry.
func tracefromEntries(vcs vulncheck.CallStack) []*govulncheck.Frame {
	var frames []*govulncheck.Frame
	for i := len(vcs) - 1; i >= 0; i-- {
		e := vcs[i]
		fr := frameFromPackage(e.Function.Package)
		fr.Function = e.Function.Name
		fr.Receiver = e.Function.Receiver()
		if e.Call == nil || e.Call.Pos == nil {
			fr.Position = nil
		} else {
			fr.Position = &govulncheck.Position{
				Filename: e.Call.Pos.Filename,
				Offset:   e.Call.Pos.Offset,
				Line:     e.Call.Pos.Line,
				Column:   e.Call.Pos.Column,
			}
		}
		frames = append(frames, fr)
	}
	return frames
}

func frameFromPackage(pkg *packages.Package) *govulncheck.Frame {
	fr := &govulncheck.Frame{}
	if pkg != nil {
		fr.Module = pkg.Module.Path
		fr.Version = pkg.Module.Version
		fr.Package = pkg.PkgPath
	}
	if pkg.Module.Replace != nil {
		fr.Module = pkg.Module.Replace.Path
		fr.Version = pkg.Module.Replace.Version
	}
	return fr
}

// sourceProgressMessage returns a string of the form
//
//	"Scanning your code and P packages across M dependent modules for known vulnerabilities..."
//
// P is the number of strictly dependent packages of
// topPkgs and Y is the number of their modules.
func sourceProgressMessage(topPkgs []*packages.Package) *govulncheck.Progress {
	pkgs, mods := depPkgsAndMods(topPkgs)

	pkgsPhrase := fmt.Sprintf("%d package", pkgs)
	if pkgs != 1 {
		pkgsPhrase += "s"
	}

	modsPhrase := fmt.Sprintf("%d dependent module", mods)
	if mods != 1 {
		modsPhrase += "s"
	}

	msg := fmt.Sprintf("Scanning your code and %s across %s for known vulnerabilities...", pkgsPhrase, modsPhrase)
	return &govulncheck.Progress{Message: msg}
}

// depPkgsAndMods returns the number of packages that
// topPkgs depend on and the number of their modules.
func depPkgsAndMods(topPkgs []*packages.Package) (int, int) {
	tops := make(map[string]bool)
	depPkgs := make(map[string]bool)
	depMods := make(map[string]bool)

	for _, t := range topPkgs {
		tops[t.PkgPath] = true
	}

	var visit func(*packages.Package, bool)
	visit = func(p *packages.Package, top bool) {
		path := p.PkgPath
		if depPkgs[path] {
			return
		}
		if tops[path] && !top {
			// A top package that is a dependency
			// will not be in depPkgs, so we skip
			// reiterating on it here.
			return
		}

		// We don't count a top-level package as
		// a dependency even when they are used
		// as a dependent package.
		if !tops[path] {
			depPkgs[path] = true
			if p.Module != nil &&
				p.Module.Path != internal.GoStdModulePath && // no module for stdlib
				p.Module.Path != internal.UnknownModulePath { // no module for unknown
				depMods[p.Module.Path] = true
			}
		}

		for _, d := range p.Imports {
			visit(d, false)
		}
	}

	for _, t := range topPkgs {
		visit(t, true)
	}

	return len(depPkgs), len(depMods)
}

// updateInitPositions populates non-existing positions of init functions
// and their respective calls in callStacks (see #51575).
func updateInitPositions(callStacks map[*vulncheck.Vuln][]vulncheck.CallStack) {
	for _, css := range callStacks {
		for _, cs := range css {
			for i := range cs {
				updateInitPosition(&cs[i])
				if i != len(cs)-1 {
					updateInitCallPosition(&cs[i], cs[i+1])
				}
			}
		}
	}
}

// updateInitCallPosition updates the position of a call to init in a stack frame, if
// one already does not exist:
//
//	P1.init -> P2.init: position of call to P2.init is the position of "import P2"
//	statement in P1
//
//	P.init -> P.init#d: P.init is an implicit init. We say it calls the explicit
//	P.init#d at the place of "package P" statement.
func updateInitCallPosition(curr *vulncheck.StackEntry, next vulncheck.StackEntry) {
	call := curr.Call
	if !isInit(next.Function) || (call.Pos != nil && call.Pos.IsValid()) {
		// Skip non-init functions and inits whose call site position is available.
		return
	}

	var pos token.Position
	if curr.Function.Name == "init" && curr.Function.Package == next.Function.Package {
		// We have implicit P.init calling P.init#d. Set the call position to
		// be at "package P" statement position.
		pos = packageStatementPos(curr.Function.Package)
	} else {
		// Choose the beginning of the import statement as the position.
		pos = importStatementPos(curr.Function.Package, next.Function.Package.PkgPath)
	}

	call.Pos = &pos
}

func importStatementPos(pkg *packages.Package, importPath string) token.Position {
	var importSpec *ast.ImportSpec
spec:
	for _, f := range pkg.Syntax {
		for _, impSpec := range f.Imports {
			// Import spec paths have quotation marks.
			impSpecPath, err := strconv.Unquote(impSpec.Path.Value)
			if err != nil {
				panic(fmt.Sprintf("import specification: package path has no quotation marks: %v", err))
			}
			if impSpecPath == importPath {
				importSpec = impSpec
				break spec
			}
		}
	}

	if importSpec == nil {
		// for sanity, in case of a wild call graph imprecision
		return token.Position{}
	}

	// Choose the beginning of the import statement as the position.
	return pkg.Fset.Position(importSpec.Pos())
}

func packageStatementPos(pkg *packages.Package) token.Position {
	if len(pkg.Syntax) == 0 {
		return token.Position{}
	}
	// Choose beginning of the package statement as the position. Pick
	// the first file since it is as good as any.
	return pkg.Fset.Position(pkg.Syntax[0].Package)
}

// updateInitPosition updates the position of P.init function in a stack frame if one
// is not available. The new position is the position of the "package P" statement.
func updateInitPosition(se *vulncheck.StackEntry) {
	fun := se.Function
	if !isInit(fun) || (fun.Pos != nil && fun.Pos.IsValid()) {
		// Skip non-init functions and inits whose position is available.
		return
	}

	pos := packageStatementPos(fun.Package)
	fun.Pos = &pos
}

func isInit(f *vulncheck.FuncNode) bool {
	// A source init function, or anonymous functions used in inits, will
	// be named "init#x" by vulncheck (more precisely, ssa), where x is a
	// positive integer. Implicit inits are named simply "init".
	return f.Name == "init" || strings.HasPrefix(f.Name, "init#")
}

// uniqueCallStack returns the first unique call stack among css, if any.
// Unique means that the call stack does not go through symbols of vg.
func uniqueCallStack(v *vulncheck.Vuln, css []vulncheck.CallStack, vg []*vulncheck.Vuln) vulncheck.CallStack {
	vulnFuncs := make(map[*vulncheck.FuncNode]bool)
	for _, v := range vg {
		vulnFuncs[v.CallSink] = true
	}

callstack:
	for _, cs := range css {
		for _, e := range cs {
			if e.Function != v.CallSink && vulnFuncs[e.Function] {
				continue callstack
			}
		}
		return cs
	}
	return nil
}
