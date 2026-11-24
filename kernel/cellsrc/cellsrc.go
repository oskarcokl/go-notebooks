// Package cellsrc splits the source of a notebook code cell into top-level
// chunks (imports, declarations and statements) and classifies cells.
package cellsrc

import (
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"strconv"
	"strings"
)

// Kind is the syntactic class of a chunk.
type Kind int

// Chunk kinds.
const (
	Import Kind = iota
	Const
	Type
	Var
	Func
	Stmt
)

func (k Kind) String() string {
	return [...]string{"import", "const", "type", "var", "func", "stmt"}[k]
}

// Chunk is one top-level declaration or statement of a cell.
type Chunk struct {
	Kind      Kind
	Text      string
	Line, Col int // 1-based position of Text inside the cell
}

// Split cuts src at its top-level statement boundaries, using the token
// stream only, so it works for any mix of declarations and statements.
func Split(src string) ([]Chunk, error) {
	fset := token.NewFileSet()
	file := fset.AddFile("cell", -1, len(src))
	var sc scanner.Scanner
	var scanErr error
	sc.Init(file, []byte(src), func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%d:%d: %s", pos.Line, pos.Column, msg)
		}
	}, 0)

	var chunks []Chunk
	var toks []token.Token
	start, end, depth := -1, 0, 0
	header := false // the chunk has a for, if or switch at its top level
	flush := func() {
		if start >= 0 && len(toks) > 0 {
			p := file.Position(file.Pos(start))
			chunks = append(chunks, Chunk{Kind: kindOf(toks), Text: src[start:end], Line: p.Line, Col: p.Column})
		}
		start, toks = -1, nil
	}
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		off := file.Offset(pos)
		if tok == token.SEMICOLON && depth == 0 {
			// "for i := 0;" is not a whole statement yet.
			if header && !complete(src[start:end]) {
				toks, end = append(toks, tok), off+len(lit)
				if lit == "\n" {
					end = off
				}
				continue
			}
			flush()
			header = false
			continue
		}
		switch tok {
		case token.FOR, token.IF, token.SWITCH:
			header = header || depth == 0
		case token.LBRACE, token.LPAREN, token.LBRACK:
			depth++
		case token.RBRACE, token.RPAREN, token.RBRACK:
			if depth--; depth < 0 {
				p := file.Position(pos)
				return nil, fmt.Errorf("%d:%d: unexpected %q", p.Line, p.Column, tok)
			}
		}
		if start < 0 {
			start = off
		}
		toks = append(toks, tok)
		if tok == token.SEMICOLON {
			if lit == ";" {
				end = off + 1
			}
		} else if lit != "" {
			end = off + len(lit)
		} else {
			end = off + len(tok.String())
		}
	}
	flush()
	if scanErr != nil {
		return nil, scanErr
	}
	if depth > 0 {
		return nil, fmt.Errorf("%d:1: unexpected end of cell: missing closing bracket", strings.Count(src, "\n")+1)
	}
	return chunks, nil
}

func complete(stmt string) bool {
	_, err := parser.ParseExpr("func() {" + stmt + "\n}")
	return err == nil
}

func kindOf(toks []token.Token) Kind {
	switch toks[0] {
	case token.IMPORT:
		return Import
	case token.CONST:
		return Const
	case token.TYPE:
		return Type
	case token.VAR:
		return Var
	case token.FUNC:
		if len(toks) > 1 && toks[1] == token.IDENT {
			return Func
		}
		// A method is "func (recv) Name(" or "func (recv) Name[";
		// anything else is an expression starting with a function literal.
		depth := 0
		for i := 1; i < len(toks); i++ {
			switch toks[i] {
			case token.LPAREN, token.LBRACK, token.LBRACE:
				depth++
			case token.RPAREN, token.RBRACK, token.RBRACE:
				if depth--; depth == 0 {
					if i+2 < len(toks) && toks[i+1] == token.IDENT && toks[i+2] == token.LPAREN {
						return Func
					}
					return Stmt
				}
			}
		}
	}
	return Stmt
}

// Info summarizes what a cell contains.
type Info struct {
	HasImport, HasFunc, HasStmt, HasDecl bool
}

// Classify reports what kinds of chunks src contains. Unparsable source is
// reported as err.
func Classify(src string) (Info, error) {
	var in Info
	chunks, err := Split(src)
	for _, c := range chunks {
		switch c.Kind {
		case Import:
			in.HasImport = true
		case Func:
			in.HasFunc = true
		case Stmt:
			in.HasStmt = true
		default:
			in.HasDecl = true
		}
	}
	return in, err
}

// Imports lists the import paths declared in src.
func Imports(src string) []string {
	chunks, _ := Split(src)
	var out []string
	for _, c := range chunks {
		if c.Kind != Import {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+c.Text, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, s := range f.Imports {
			if p, err := strconv.Unquote(s.Path.Value); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}
