package main

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newMCP is an MCP server named name, for tools to be added to.
func newMCP(name string) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.1.0"}, nil)
}

// mcpHandler serves an MCP server over Streamable HTTP in its plain form: no sessions, and each
// request answered with one JSON body, since no tool has anything to say before its one answer.
func mcpHandler(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

// said is a tool's answer, as text for the agent to read. A tool's error is its answer too, said
// as one.
func said(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

// The arguments the task tools take, Devin's and OpenCode's subagents' alike.
type (
	peekArgs struct {
		SessionID string `json:"sessionID" jsonschema:"The subagent's session, as the subagent tool gave it."`
		Lines     int    `json:"lines,omitempty" jsonschema:"How many of the latest lines to return. Default 40, at most 200."`
	}
	stopArgs struct {
		SessionID      string  `json:"sessionID" jsonschema:"The subagent's session, as the subagent tool gave it."`
		TimeoutSeconds float64 `json:"timeout_seconds,omitempty" jsonschema:"How long it has to wrap up before it is cut short. Default 60."`
	}
)

// peekLines is how many lines a peek asks for, within bounds.
func (a peekArgs) peekLines() int {
	if a.Lines <= 0 {
		return 40
	}
	return min(a.Lines, 200)
}

// timeout is how long a stop gives, in seconds.
func (a stopArgs) timeout() float64 {
	if a.TimeoutSeconds <= 0 {
		return 60
	}
	return a.TimeoutSeconds
}
