//go:build tools

// Package tools pins development tool dependencies so they are tracked in
// go.mod and installed at reproducible versions.
package tools

import (
	_ "github.com/swaggo/swag/cmd/swag"
)
