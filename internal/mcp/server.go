// Package mcp exposes the archive service as read-only MCP tools.
package mcp

import (
	"context"
	"net/http"

	"github.com/bitskyai/chatkeeper/internal/search"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func New(s *search.Service) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "chatkeeper", Version: "0.3.0"}, nil)
	no := false
	annotations := &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no, OpenWorldHint: &no, IdempotentHint: true}
	sdk.AddTool(server, &sdk.Tool{Name: "search_threads", Description: "Search verified archived threads using FTS5. Includes retained deleted content and prior edits; flags indicate their presence. after is an opaque cursor for the same query and unchanged indexes.", Annotations: annotations}, func(ctx context.Context, _ *sdk.CallToolRequest, input searchInput) (*sdk.CallToolResult, search.Page, error) {
		q := search.Query{Q: input.Q, Workspace: input.Workspace, Channel: input.Channel, Author: input.Author, From: input.From, To: input.To, After: input.After}
		if input.Limit != nil {
			if *input.Limit < 1 || *input.Limit > 100 {
				return nil, search.Page{}, search.ErrQuery
			}
			q.Limit = *input.Limit
		}
		p, e := s.Search(ctx, q)
		return nil, p, e
	})
	type getInput struct {
		Workspace string `json:"workspace"`
		Channel   string `json:"channel"`
		TS        string `json:"ts"`
	}
	sdk.AddTool(server, &sdk.Tool{Name: "get_thread", Description: "Read verified archived JSON including schema-2 history; no binary attachments.", Annotations: annotations}, func(ctx context.Context, _ *sdk.CallToolRequest, q getInput) (*sdk.CallToolResult, search.Thread, error) {
		v, e := s.GetThread(ctx, q.Workspace, q.Channel, q.TS)
		return nil, v, e
	})
	type empty struct{}
	type listOutput struct {
		Workspaces []string `json:"workspaces"`
	}
	sdk.AddTool(server, &sdk.Tool{Name: "list_workspaces", Description: "List configured workspace IDs, without credentials or storage locations.", Annotations: annotations}, func(context.Context, *sdk.CallToolRequest, empty) (*sdk.CallToolResult, listOutput, error) {
		return nil, listOutput{s.ListWorkspaces()}, nil
	})
	return server
}
func HTTP(s *search.Service) http.Handler {
	server := New(s)
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

type searchInput struct {
	Q         string `json:"q" jsonschema:"SQLite FTS5 query (1 to 4096 characters)"`
	Workspace string `json:"workspace,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Author    string `json:"author,omitempty"`
	From      string `json:"from,omitempty" jsonschema:"Inclusive root time as epoch seconds or ISO 8601"`
	To        string `json:"to,omitempty" jsonschema:"Exclusive root time as epoch seconds or ISO 8601"`
	Limit     *int   `json:"limit,omitempty" jsonschema:"Page size from 1 to 100 (default 20)"`
	After     string `json:"after,omitempty" jsonschema:"Opaque continuation for unchanged query and index"`
}
