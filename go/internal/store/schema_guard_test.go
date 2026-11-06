package store

import (
	"context"
	"strings"
	"testing"
)

// A restore compares definitions with whitespace collapsed. That's only safe while no definition holds a
// comment, which a newline ends, or spaces inside quotes, which are part of a value or a name. A migration that
// adds either has to change how definitions are compared first.
func TestNoDefinitionHoldsACommentOrSpacesInQuotes(t *testing.T) {
	levels, err := MigrationCount()
	if err != nil {
		t.Fatal(err)
	}
	for level := 0; level <= levels; level++ {
		shape, err := ExpectedShape(context.Background(), level)
		if err != nil {
			t.Fatal(err)
		}
		for key, d := range shape.Definitions {
			if strings.Contains(d.SQL, "--") || strings.Contains(d.SQL, "/*") {
				t.Errorf("level %d: %s holds a comment", level, key)
			}
			var quote rune
			for _, r := range d.SQL {
				switch {
				case quote == 0 && (r == '\'' || r == '"' || r == '`' || r == '['):
					quote = r
					if r == '[' {
						quote = ']'
					}
				case quote != 0 && r == quote:
					quote = 0
				case quote != 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r'):
					t.Errorf("level %d: %s holds whitespace inside quotes", level, key)
				}
			}
		}
	}
}
