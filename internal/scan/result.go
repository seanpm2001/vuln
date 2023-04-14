// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package govulncheck provides functionality to support the govulncheck command.
package scan

import (
	"fmt"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/vuln/internal/govulncheck"
)

// LoadMode is the level of information needed for each package
// for running golang.org/x/tools/go/packages.Load.
var LoadMode = packages.NeedName | packages.NeedImports | packages.NeedTypes |
	packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedDeps |
	packages.NeedModule

// IsCalled reports whether the vulnerability is called, therefore
// affecting the target source code or binary.
func IsCalled(v *govulncheck.Vuln) bool {
	for _, m := range v.Modules {
		for _, p := range m.Packages {
			if len(p.CallStacks) > 0 {
				return true
			}
		}
	}
	return false
}

// FuncName returns the full qualified function name from a stack frame,
// adjusted to remove pointer annotations.
func FuncName(frame *govulncheck.StackFrame) string {
	switch {
	case frame.Receiver != "":
		return fmt.Sprintf("%s.%s", strings.TrimPrefix(frame.Receiver, "*"), frame.Function)
	case frame.Package != "":
		return fmt.Sprintf("%s.%s", frame.Package, frame.Function)
	default:
		return frame.Function
	}
}

// Pos returns the position of the call in sf as string.
// If position is not available, return "".
func Pos(sf *govulncheck.StackFrame) string {
	p := sf.Position.ToTokenPosition()
	if p != nil && p.IsValid() {
		return p.String()
	}
	return ""
}
