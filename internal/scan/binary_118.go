// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.18
// +build go1.18

package scan

import (
	"context"
	"io"

	"golang.org/x/vuln/internal/client"
	"golang.org/x/vuln/internal/govulncheck"
	"golang.org/x/vuln/internal/vulncheck"
)

func binary(ctx context.Context, exe io.ReaderAt, cfg *govulncheck.Config, client *client.Client) (_ *vulncheck.Result, err error) {
	return vulncheck.Binary(ctx, exe, cfg, client)
}
