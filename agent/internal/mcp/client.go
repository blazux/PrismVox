// Package mcp provides MCP (Model Context Protocol) tool calling via SSE/HTTP transports.
//
// Stdio transport is not implemented in v1 — a warning is logged and an error returned.
// Streamable HTTP and persistent legacy SSE use initialized MCP sessions.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"phoneagent/internal/secrets"
)

// Tool describes a tool exposed by an MCP server.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
}

type toolEntry struct {
	Tool
	client *rpcSession
}

// Manager manages connections to MCP servers and their tools.
type Manager struct {
	mu    sync.RWMutex
	tools map[string]toolEntry
}

// NewManager creates a new MCP Manager.
func NewManager() *Manager {
	return &Manager{
		tools: make(map[string]toolEntry),
	}
}

// ConnectAll connects to all enabled MCP servers from the database.
func (m *Manager) ConnectAll(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		"SELECT id, name, type, url, auth_token FROM mcp_servers WHERE enabled = TRUE",
	)
	if err != nil {
		return fmt.Errorf("mcp: query servers: %w", err)
	}
	defer rows.Close()

	type serverRow struct {
		id        int64
		name      string
		mtype     string
		url       *string
		authToken *string
	}

	var servers []serverRow
	for rows.Next() {
		var s serverRow
		if err := rows.Scan(&s.id, &s.name, &s.mtype, &s.url, &s.authToken); err != nil {
			log.Printf("mcp: scan server row: %v", err)
			continue
		}
		servers = append(servers, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	for _, s := range servers {
		if err := m.connectServer(ctx, pool, s.id, s.name, s.mtype, s.url, s.authToken); err != nil {
			log.Printf("mcp: connect '%s' failed: %v", s.name, err)
		}
	}
	return nil
}

func (m *Manager) connectServer(
	ctx context.Context,
	pool *pgxpool.Pool,
	serverID int64,
	name, mtype string,
	urlPtr *string,
	authTokenPtr *string,
) error {
	if mtype == "stdio" {
		log.Printf("mcp: server '%s' uses stdio transport — not implemented in v1, skipping", name)
		return fmt.Errorf("stdio transport not implemented")
	}

	if urlPtr == nil || *urlPtr == "" {
		return fmt.Errorf("mcp server '%s' has no URL", name)
	}
	serverURL := *urlPtr
	var authToken string
	if authTokenPtr != nil {
		var err error
		authToken, err = secrets.Decrypt(*authTokenPtr, fmt.Sprintf("mcp:%d", serverID))
		if err != nil {
			return err
		}
	}

	client, err := openSession(ctx, serverURL, authToken, mtype)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			client.Close()
		}
	}()
	toolList, err := listSessionTools(ctx, client)
	if err != nil {
		return err
	}
	if err = m.upsertTools(ctx, pool, serverID, toolList); err != nil {
		return err
	}
	enabledTools, err := m.getEnabledTools(ctx, pool, serverID)
	if err != nil {
		return err
	}

	loaded := 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range toolList {
		if _, enabled := enabledTools[t.Name]; enabledTools != nil && !enabled {
			continue
		}
		if _, exists := m.tools[t.Name]; exists {
			return fmt.Errorf("duplicate MCP tool name %q", t.Name)
		}
	}
	for _, t := range toolList {
		if enabledTools != nil {
			if _, ok := enabledTools[t.Name]; !ok {
				continue
			}
		}
		m.tools[t.Name] = toolEntry{
			Tool:   t,
			client: client,
		}
		loaded++
	}
	accepted = loaded > 0

	log.Printf("mcp: '%s': %d/%d tools active", name, loaded, len(toolList))
	return nil
}

// getEnabledTools returns the set of enabled tool names for a server,
// or nil if no tools have been recorded yet (meaning allow all).
func (m *Manager) getEnabledTools(ctx context.Context, pool *pgxpool.Pool, serverID int64) (map[string]struct{}, error) {
	var total int64
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM mcp_tools WHERE server_id = $1", serverID,
	).Scan(&total)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil // not yet probed — allow all
	}

	rows, err := pool.Query(ctx,
		"SELECT name FROM mcp_tools WHERE server_id = $1 AND enabled = TRUE", serverID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			result[name] = struct{}{}
		}
	}
	return result, rows.Err()
}

// listSessionTools discovers the entire catalogue before exposing any tools.
func listSessionTools(ctx context.Context, client *rpcSession) ([]Tool, error) {
	var tools []Tool
	seen := map[string]bool{}
	names := map[string]bool{}
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]interface{}{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := client.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		raw, ok := result["tools"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("MCP catalogue has no tools array")
		}
		for _, v := range raw {
			item, ok := v.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("invalid MCP tool")
			}
			name := stringVal(item, "name")
			schema, ok := item["inputSchema"].(map[string]interface{})
			if name == "" || names[name] || !ok {
				return nil, fmt.Errorf("invalid or duplicate MCP tool definition")
			}
			names[name] = true
			tools = append(tools, Tool{Name: name, Description: stringVal(item, "description"), InputSchema: schema})
		}
		cursor = stringVal(result, "nextCursor")
		if cursor == "" {
			return tools, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("repeated MCP cursor")
		}
		seen[cursor] = true
	}
	return nil, fmt.Errorf("MCP catalogue pagination limit exceeded")
}

// upsertTools persists tool metadata to the database.
func (m *Manager) upsertTools(ctx context.Context, pool *pgxpool.Pool, serverID int64, tools []Tool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, t := range tools {
		_, err := tx.Exec(ctx,
			`INSERT INTO mcp_tools (server_id, name, description)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (server_id, name) DO UPDATE SET description = EXCLUDED.description`,
			serverID, t.Name, t.Description,
		)
		if err != nil {
			return err
		}
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM mcp_tools WHERE server_id=$1 AND NOT(name=ANY($2::text[]))", serverID, names); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Tools returns the available MCP tools in OpenAI function format.
func (m *Manager) Tools() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.tools) == 0 {
		return nil
	}

	tools := make([]map[string]interface{}, 0, len(m.tools))
	for _, t := range m.tools {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.InputSchema,
			},
		})
	}
	return tools
}

// CallTool executes a named tool with the given arguments.
func (m *Manager) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	m.mu.RLock()
	entry, ok := m.tools[name]
	m.mu.RUnlock()

	if !ok {
		return fmt.Sprintf("Error: unknown tool '%s'", name), fmt.Errorf("unknown tool %q", name)
	}

	params := map[string]interface{}{
		"name":      name,
		"arguments": args,
	}

	result, err := entry.client.call(ctx, "tools/call", params)
	if err != nil {
		return fmt.Sprintf("Tool error: %v", err), err
	}

	var parts []string
	if content, ok := result["content"].([]interface{}); ok {
		for _, v := range content {
			if item, ok := v.(map[string]interface{}); ok {
				if text, ok := item["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
	}
	output := ""
	if len(parts) > 0 {
		output = strings.Join(parts, "\n")
	} else {
		b, _ := json.Marshal(result)
		output = string(b)
	}
	if failed, _ := result["isError"].(bool); failed {
		return output, fmt.Errorf("MCP tool reported an error")
	}
	return output, nil
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	clients := map[*rpcSession]bool{}
	for _, entry := range m.tools {
		clients[entry.client] = true
	}
	m.tools = make(map[string]toolEntry)
	m.mu.Unlock()
	for client := range clients {
		client.Close()
	}
}

func stringVal(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
