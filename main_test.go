package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLogMessageStyle locks the log vocabulary: every record emitted through the
// global logger uses a lowercase single-line message, so one grep pattern works
// across export/import/shard/worker lines and the java/text layouts stay
// consistent. The previous mix ("Export completed" next to "export
// configuration" and "Starting export") meant a case-sensitive grep silently
// missed half the records, and two lines reported the same event.
func TestLogMessageStyle(t *testing.T) {
	skipDirs := map[string]bool{"bin": true, ".git": true, "docs": true, "test": true}
	checked := 0

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isLoggerReceiver(sel.X) {
				return true
			}
			switch sel.Sel.Name {
			case "Debug", "Info", "Warn", "Error":
			default:
				return true
			}
			if len(call.Args) == 0 {
				t.Errorf("%s: logger.%s needs a message", path, sel.Sel.Name)
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				// A computed message (logger.Warn(msg)) is still one line, but the
				// case rule cannot be checked statically; let it through.
				return true
			}
			msg, err := strconv.Unquote(lit.Value)
			if err != nil || msg == "" {
				return true
			}
			checked++
			if msg[0] >= 'A' && msg[0] <= 'Z' {
				t.Errorf("%s: logger.%s message %q must start lower case (vocabulary is lowercase <object> <event>)",
					path, sel.Sel.Name, msg)
			}
			if strings.ContainsAny(msg, "\n\r") {
				t.Errorf("%s: logger.%s message %q must stay on one line", path, sel.Sel.Name, msg)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk sources: %v", err)
	}
	if checked < 30 {
		t.Fatalf("only %d logger call sites inspected, the walk probably missed the sources", checked)
	}
}

// isLoggerReceiver reports whether x names the package-level logger, written as
// common.Logger.X(...) in cmd/* and internal/*, or Logger.X(...) inside
// internal/common itself.
func isLoggerReceiver(x ast.Expr) bool {
	if id, ok := x.(*ast.Ident); ok {
		return id.Name == "Logger"
	}
	if sel, ok := x.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok {
			return id.Name == "common" && sel.Sel.Name == "Logger"
		}
	}
	return false
}
