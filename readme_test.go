package entitlements_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The README quickstart is pasted from the compiled Examples. This test keeps
// the two identical: a fenced block after `<!-- example:Name -->` must equal
// the body of that function in quickstart_example_test.go or
// apple/serverapi/example_test.go.
func TestReadmeSnippetsMatchExamples(t *testing.T) {
	bodies := map[string]string{}
	for _, name := range []string{"quickstart_example_test.go", "apple/serverapi/example_test.go"} {
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "Example") {
				continue
			}
			raw := string(src[fset.Position(fn.Body.Lbrace).Offset+1 : fset.Position(fn.Body.Rbrace).Offset])
			lines := strings.Split(strings.Trim(raw, "\n"), "\n")
			for i, l := range lines {
				lines[i] = strings.TrimPrefix(l, "\t")
			}
			bodies[fn.Name.Name] = strings.Join(lines, "\n")
		}
	}

	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	re := regexp.MustCompile("(?s)<!-- example:(\\w+) -->\n```go\n(.*?)\n```")
	matches := re.FindAllStringSubmatch(string(readme), -1)
	require.NotEmpty(t, matches, "README has no example markers")
	for _, m := range matches {
		want, ok := bodies[m[1]]
		require.True(t, ok, "README refers to unknown example %s", m[1])
		require.Equal(t, want, m[2], "README snippet for %s differs from its compiled Example", m[1])
	}
}
