package test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Advisory gate: reports numeric literals standing in expressions.
//
// It never fails the build. That is a deliberate decision, and the reason is
// that a gate which fails is a gate that gets disabled, and one that gets
// disabled protects nothing. This one is here to be run, read, and argued with:
//
//	go test ./test/ -run MagicNumbers -v
//
// The class it looks for is narrow on purpose. What it flags is a bare number
// whose MEANING lives in a header or a datasheet -- an ioctl capability bit, a
// protocol field width, a wire offset. Those are the ones where a reader cannot
// tell what the value is, and where a wrong value produces no error message,
// just wrong behaviour on the bench.
//
// What it deliberately does NOT flag:
//
//   - Any literal bound to a name, `const` or `var`. The VIDIOC codes cannot be
//     const because unsafe.Sizeof is not a constant expression, and they are as
//     named and as commented as any constant in the tree.
//   - Array lengths in type declarations. `Driver [16]uint8` is ABI, but the
//     field carries the comment and a name would only move the number away from
//     the thing it describes.
//   - Named struct-literal fields. `Timeout: 5 * time.Second` is configuration
//     and naming it is a style preference, not a correctness question.
//   - 0, 1 and 2, which are overwhelmingly loop counters, indices and boolean
//     results, and which would bury the rest.
//
// The exclusions are why this can be advisory. A gate that flagged every number
// in the tree would produce hundreds of lines on day one and be switched off
// within a week, and the four lines that mattered would go with it.
func TestMagicNumbers(t *testing.T) {
	root := repoRoot(t)

	type hit struct {
		line int
		text string
	}

	byFile := map[string][]hit{}
	total := 0

	for _, dir := range []string{"internal", "cmd", "tools"} {
		base := filepath.Join(root, dir)
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil //nolint:nilerr // an unreadable dir just yields no files
			}
			rel, _ := filepath.Rel(root, path)

			// Generated code and test code are not ours to tidy.
			if strings.HasSuffix(path, "_test.go") || strings.Contains(rel, string(filepath.Separator)+"gen"+string(filepath.Separator)) {
				return nil
			}

			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				return nil //nolint:nilerr // unparseable files are not this test's business
			}
			for _, cg := range file.Comments {
				if strings.Contains(cg.Text(), "Code generated") && strings.Contains(cg.Text(), "DO NOT EDIT") {
					return nil
				}
			}

			var lines []hit
			ast.Inspect(file, func(n ast.Node) bool {
				// A const decl is the fix, and a literal inside it is already
				// named.
				if gd, ok := n.(*ast.GenDecl); ok && gd.Tok == token.CONST {
					return false
				}
				// A `var` block is not automatically worse than a `const` one.
				// The VIDIOC request codes cannot be const -- unsafe.Sizeof is not
				// a constant expression -- and they are named, commented, and as
				// readable as constants get. What matters is whether the literal
				// is bound to something a reader can use, not which keyword
				// introduced it. This was 30 false positives on its own.
				if vs, ok := n.(*ast.ValueSpec); ok && len(vs.Names) > 0 && vs.Names[0].Name != "_" {
					return false
				}
				// An array length is a shape, not a setting. `Driver [16]uint8`
				// is ABI-derived, but the field it belongs to carries the comment
				// that says so, and `driverNameLen` would only move the number
				// somewhere less close to what it describes.
				if _, ok := n.(*ast.ArrayType); ok {
					return false
				}
				// A named field's value is configuration, not an ABI constant.
				if kv, ok := n.(*ast.KeyValueExpr); ok && kv.Key != nil {
					if _, isIdent := kv.Key.(*ast.Ident); isIdent {
						return false
					}
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || (lit.Kind != token.INT && lit.Kind != token.FLOAT && lit.Kind != token.IMAG) {
					return true
				}
				if trivial(lit.Value) {
					return true
				}
				pos := fset.Position(lit.Pos())
				lines = append(lines, hit{pos.Line, lit.Value})
				return true
			})

			if len(lines) > 0 {
				byFile[rel] = lines
				total += len(lines)
			}
			return nil
		})
	}

	if total == 0 {
		t.Log("no bare numeric literals in expressions outside const blocks")
		return
	}

	t.Logf("%d bare numeric literal(s) in expressions. Advisory only -- naming one "+
		"is worth it when the meaning is in a header, a datasheet or a wire format:\n", total)

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	for _, f := range files {
		lines := byFile[f]
		sort.Slice(lines, func(i, j int) bool { return lines[i].line < lines[j].line })
		t.Logf("  %s  (%d)", f, len(lines))
		for _, h := range lines {
			t.Logf("    %s:%d  %s", f, h.line, h.text)
		}
	}
}

// trivial reports whether a literal is one of the values that carry no meaning
// on their own and would be pure noise.
//
// 0, 1 and 2 are excluded because they are almost always an index, a loop bound,
// a "not found" sentinel or a boolean-as-integer. -1 reaches here as a UnaryExpr
// over 1 and is excluded by the same rule.
func trivial(lit string) bool {
	switch lit {
	case "0", "1", "2":
		return true
	}
	// Hex and octal literals of small values, written as 0x00000001 and friends.
	if v, err := strconv.ParseInt(lit, 0, 64); err == nil {
		switch v {
		case 0, 1, 2:
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// This file lives in test/, so the module root is one level up.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}
