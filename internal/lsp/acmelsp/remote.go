package acmelsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"9fans.net/acme-lsp/internal/acmeutil"
	"9fans.net/acme-lsp/internal/lsp"
	"9fans.net/acme-lsp/internal/lsp/proxy"
	"9fans.net/acme-lsp/internal/lsp/text"
	"9fans.net/internal/go-lsp/lsp/protocol"
)

// RemoteCmd executes LSP commands in an acme window using the proxy server.
type RemoteCmd struct {
	server proxy.Server
	winid  int
	Stdout io.Writer
	Stderr io.Writer
}

func NewRemoteCmd(server proxy.Server, winid int) *RemoteCmd {
	return &RemoteCmd{
		server: server,
		winid:  winid,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

func (rc *RemoteCmd) getPosition() (pos *protocol.TextDocumentPositionParams, filename string, err error) {
	w, err := acmeutil.OpenWin(rc.winid)
	if err != nil {
		return nil, "", fmt.Errorf("failed to to open window %v: %v", rc.winid, err)
	}
	defer w.CloseFiles()

	return text.Position(w)
}

func (rc *RemoteCmd) DidChange(ctx context.Context) error {
	w, err := acmeutil.OpenWin(rc.winid)
	if err != nil {
		return fmt.Errorf("failed to to open window %v: %v", rc.winid, err)
	}
	defer w.CloseFiles()

	uri, _, err := text.DocumentURI(w)
	if err != nil {
		return err
	}
	body, err := w.ReadAll("body")
	if err != nil {
		return err
	}

	return rc.server.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{
				URI: uri,
			},
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			{
				Text: string(body),
			},
		},
	})
}

type CompletionKind int

const (
	CompleteNoEdit CompletionKind = iota
	CompleteInsertOnlyMatch
	CompleteInsertFirstMatch
)

func (rc *RemoteCmd) Completion(ctx context.Context, kind CompletionKind) error {
	w, err := acmeutil.OpenWin(rc.winid)
	if err != nil {
		return err
	}
	defer w.CloseFiles()

	pos, _, err := text.Position(w)
	if err != nil {
		return err
	}
	result, err := rc.server.Completion(ctx, &protocol.CompletionParams{
		TextDocumentPositionParams: *pos,
	})
	if err != nil {
		return err
	}

	if (kind == CompleteInsertFirstMatch && len(result.Items) >= 1) || (kind == CompleteInsertOnlyMatch && len(result.Items) == 1) {
		textEdit := result.Items[0].TextEdit
		if textEdit == nil {
			// TODO(fhs): Use insertText or label instead.
			return fmt.Errorf("nil TextEdit in completion item")
		}
		if err := text.Edit(w, []protocol.TextEdit{*textEdit}); err != nil {
			return fmt.Errorf("failed to apply completion edit: %v", err)
		}

		if len(result.Items) == 1 {
			return nil
		}
	}

	var sb strings.Builder

	if len(result.Items) == 0 {
		fmt.Fprintf(&sb, "no completion\n")
	}

	for _, item := range result.Items {
		fmt.Fprintf(&sb, "%v\t%v\n", item.Label, item.Detail)
	}

	if kind == CompleteInsertFirstMatch {
		cw, err := acmeutil.Hijack("/LSP/Completions")
		if err != nil {
			cw, err = acmeutil.NewWin()
			if err != nil {
				return err
			}

			cw.Name("/LSP/Completions")
		}

		defer cw.Win.Ctl("clean")

		cw.Clear()
		cw.PrintTabbed(sb.String())
	} else {
		fmt.Fprintln(rc.Stdout, sb.String())
	}

	return nil
}

func (rc *RemoteCmd) Definition(ctx context.Context, print bool) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return fmt.Errorf("failed to get position: %v", err)
	}
	locations, err := rc.server.Definition(ctx, &protocol.DefinitionParams{
		TextDocumentPositionParams: *pos,
	})
	if err != nil {
		return fmt.Errorf("bad server response: %v", err)
	}
	if print {
		return PrintLocations(rc.Stdout, locations)
	}
	return PlumbLocations(locations)
}

func (rc *RemoteCmd) OrganizeImportsAndFormat(ctx context.Context) error {
	win, err := acmeutil.OpenWin(rc.winid)
	if err != nil {
		return err
	}
	defer win.CloseFiles()

	uri, _, err := text.DocumentURI(win)
	if err != nil {
		return err
	}

	doc := &protocol.TextDocumentIdentifier{
		URI: uri,
	}
	return CodeActionAndFormat(ctx, rc.server, doc, win, []protocol.CodeActionKind{
		protocol.SourceOrganizeImports,
	})
}

func (rc *RemoteCmd) Hover(ctx context.Context) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return err
	}

	hov, err := rc.server.Hover(ctx, &protocol.HoverParams{
		TextDocumentPositionParams: *pos,
	})
	if err != nil {
		return err
	}

	if hov == nil {
		fmt.Fprintln(rc.Stdout, "No hover help available.")
		return nil
	}

	fmt.Fprintf(rc.Stdout, "%v\n", hov.Contents.Value)

	return nil
}

func (rc *RemoteCmd) Implementation(ctx context.Context, print bool) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return err
	}
	loc, err := rc.server.Implementation(ctx, &protocol.ImplementationParams{
		TextDocumentPositionParams: *pos,
	})
	if err != nil {
		return err
	}
	if len(loc) == 0 {
		fmt.Fprintf(rc.Stderr, "No implementations found.\n")
		return nil
	}
	return PrintLocations(rc.Stdout, loc)
}

func (rc *RemoteCmd) References(ctx context.Context) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return err
	}
	loc, err := rc.server.References(ctx, &protocol.ReferenceParams{
		TextDocumentPositionParams: *pos,
		Context: protocol.ReferenceContext{
			IncludeDeclaration: true,
		},
	})
	if err != nil {
		return err
	}
	if len(loc) == 0 {
		fmt.Fprintf(rc.Stderr, "No references found.\n")
		return nil
	}
	return PrintLocations(rc.Stdout, loc)
}

// Rename renames the identifier at cursor position to newname.
func (rc *RemoteCmd) Rename(ctx context.Context, newname string) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return err
	}
	we, err := rc.server.Rename(ctx, &protocol.RenameParams{
		TextDocument: pos.TextDocument,
		Position:     pos.Position,
		NewName:      newname,
	})
	if err != nil {
		return err
	}
	return editWorkspace(we)
}

func (rc *RemoteCmd) SignatureHelp(ctx context.Context) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return err
	}
	sh, err := rc.server.SignatureHelp(ctx, &protocol.SignatureHelpParams{
		TextDocumentPositionParams: *pos,
	})
	if err != nil {
		return err
	}
	if sh != nil {
		for _, sig := range sh.Signatures {
			fmt.Fprintf(rc.Stdout, "%v\n", sig.Label)
			fmt.Fprintf(rc.Stdout, "%v\n", sig.Documentation)
		}
	}
	return nil
}

func (rc *RemoteCmd) DocumentSymbol(ctx context.Context) error {
	win, err := acmeutil.OpenWin(rc.winid)
	if err != nil {
		return err
	}
	defer win.CloseFiles()

	basedir := "" // TODO

	uri, _, err := text.DocumentURI(win)
	if err != nil {
		return err
	}

	// TODO(fhs): DocumentSymbol request can return either a
	// []DocumentSymbol (hierarchical) or []SymbolInformation (flat).
	// We only handle the hierarchical type below.

	// TODO(fhs): Make use of DocumentSymbol.Range to optionally filter out
	// symbols that aren't within current cursor position?

	syms, err := rc.server.DocumentSymbol(ctx, &protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: uri,
		},
	})
	if err != nil {
		return err
	}
	if len(syms) == 0 {
		fmt.Fprintf(rc.Stderr, "No symbols found.\n")
		return nil
	}
	walkDocumentSymbols(syms, 0, func(s *protocol.DocumentSymbol, depth int) {
		loc := &protocol.Location{
			URI:   uri,
			Range: s.SelectionRange,
		}
		indent := strings.Repeat(" ", depth)
		fmt.Fprintf(rc.Stdout, "%v%v %v\n", indent, s.Name, s.Detail)
		fmt.Fprintf(rc.Stdout, "%v %v\n", indent, lsp.LocationLink(loc, basedir))
	})
	return nil
}

func (rc *RemoteCmd) TypeDefinition(ctx context.Context, print bool) error {
	pos, _, err := rc.getPosition()
	if err != nil {
		return err
	}
	locations, err := rc.server.TypeDefinition(ctx, &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: *pos,
	})
	if err != nil {
		return err
	}
	if print {
		return PrintLocations(rc.Stdout, locations)
	}
	return PlumbLocations(locations)
}

func walkDocumentSymbols1(syms []protocol.DocumentSymbol, depth int, f func(s *protocol.DocumentSymbol, depth int)) {
	for _, s := range syms {
		f(&s, depth)
		walkDocumentSymbols1(s.Children, depth+1, f)
	}
}

func walkDocumentSymbols(syms []interface{}, depth int, f func(s *protocol.DocumentSymbol, depth int)) {
	for _, s := range syms {
		switch val := s.(type) {
		default:
			log.Printf("unknown symbol type %T", val)

		case protocol.DocumentSymbol:
			f(&val, depth)
			walkDocumentSymbols1(val.Children, depth+1, f)

		// Workaround for the DocumentSymbol not being parsed by the auto-generated LSP definitions.
		case map[string]interface{}:
			ds, err := parseDocumentSymbol(val)
			if err != nil {
				log.Printf("failed to parse DocumentSymbols: %v\n", err)
			} else {
				f(ds, depth)
				walkDocumentSymbols1(ds.Children, depth+1, f)
			}
		}
	}
}

func parseDocumentSymbol(data map[string]interface{}) (*protocol.DocumentSymbol, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var ds protocol.DocumentSymbol
	if err := json.Unmarshal(b, &ds); err != nil {
		return nil, err
	}
	return &ds, nil
}
