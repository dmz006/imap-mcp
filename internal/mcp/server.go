// Package mcp wires all MCP tools into a single server instance.
package mcp

import (
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/mcp/tools"
	"github.com/dmz006/imap-mcp/internal/output"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/mark3labs/mcp-go/server"
)

// NewServer creates and configures the MCP server with all registered tools.
// When enforceScopes is true (HTTP transport with auth enabled), every tool
// call and tools/list is checked against the caller's token scopes.
func NewServer(
	cfg *config.Config,
	pool *imap.Pool,
	database *db.DB,
	syncer *sync.Syncer,
	out *output.Writer,
	pipeline *enrichment.Pipeline,
	enforceScopes bool,
) *server.MCPServer {
	opts := []server.ServerOption{server.WithToolCapabilities(true)}
	if enforceScopes {
		opts = append(opts,
			server.WithToolHandlerMiddleware(scopeMiddleware),
			server.WithToolFilter(scopeFilter),
		)
	}
	s := server.NewMCPServer("imap-mcp", config.Version, opts...)

	h := tools.NewHandlers(cfg, pool, database, syncer, out)
	h.SetPipeline(pipeline)

	// ── Account & connection ─────────────────────────────────────────────────
	s.AddTool(tools.ListAccountsTool(), h.ListAccounts)
	s.AddTool(tools.SyncAccountTool(), h.SyncAccount)

	// ── Folders ──────────────────────────────────────────────────────────────
	s.AddTool(tools.ListFoldersTool(), h.ListFolders)
	s.AddTool(tools.CreateFolderTool(), h.CreateFolder)
	s.AddTool(tools.DeleteFolderTool(), h.DeleteFolder)
	s.AddTool(tools.LabelMessageTool(), h.LabelMessage)
	s.AddTool(tools.EmptyTrashTool(), h.EmptyTrash)
	s.AddTool(tools.LabelBulkTool(), h.LabelBulk)

	// ── Messages (read) ──────────────────────────────────────────────────────
	s.AddTool(tools.ListMessagesTool(), h.ListMessages)
	s.AddTool(tools.GetMessageTool(), h.GetMessage)
	s.AddTool(tools.GetThreadTool(), h.GetThread)
	s.AddTool(tools.GetHeadersTool(), h.GetHeaders)
	s.AddTool(tools.GetAttachmentsTool(), h.GetAttachments)
	s.AddTool(tools.ExportMessageTool(), h.ExportMessage)

	// ── Messages (write) ─────────────────────────────────────────────────────
	s.AddTool(tools.MoveMessageTool(), h.MoveMessage)
	s.AddTool(tools.CopyMessageTool(), h.CopyMessage)
	s.AddTool(tools.DeleteMessageTool(), h.DeleteMessage)
	s.AddTool(tools.SetFlagsTool(), h.SetFlags)
	s.AddTool(tools.AppendMessageTool(), h.AppendMessage)
	s.AddTool(tools.MoveBulkTool(), h.MoveBulk)
	s.AddTool(tools.FlagBulkTool(), h.FlagBulk)
	s.AddTool(tools.PurgeSenderTool(), h.PurgeSender)

	// ── Outbound (SMTP send — per-account) ───────────────────────────────────
	s.AddTool(tools.SendMessageTool(), h.SendMessage)

	// ── Analytics ────────────────────────────────────────────────────────────
	s.AddTool(tools.TopSendersTool(), h.TopSenders)

	// ── Automation rules ─────────────────────────────────────────────────────
	s.AddTool(tools.CreateRuleTool(), h.CreateRule)
	s.AddTool(tools.ListRulesTool(), h.ListRules)
	s.AddTool(tools.DeleteRuleTool(), h.DeleteRule)
	s.AddTool(tools.RunRulesTool(), h.RunRules)

	// ── Search ───────────────────────────────────────────────────────────────
	s.AddTool(tools.SearchMessagesTool(), h.SearchMessages)
	s.AddTool(tools.CrossAccountSearchTool(), h.CrossAccountSearch)
	s.AddTool(tools.SemanticSearchTool(), h.SemanticSearch)

	// ── Intelligence ─────────────────────────────────────────────────────────
	s.AddTool(tools.SummarizeFolderTool(), h.SummarizeFolder)
	s.AddTool(tools.DetectSubscriptionsTool(), h.DetectSubscriptions)
	s.AddTool(tools.GetSenderHistoryTool(), h.GetSenderHistory)
	s.AddTool(tools.GetSenderProfileTool(), h.GetSenderProfile)
	s.AddTool(tools.KGQueryTool(), h.KGQuery)
	s.AddTool(tools.GetAnomaliesTool(), h.GetAnomalies)
	s.AddTool(tools.ResolveAnomalyTool(), h.ResolveAnomaly)
	s.AddTool(tools.NeedsReplyTool(), h.NeedsReply)
	s.AddTool(tools.AwaitingReplyTool(), h.AwaitingReply)
	s.AddTool(tools.DismissReplyTool(), h.DismissReply)
	s.AddTool(tools.EnrichmentStatusTool(), h.EnrichmentStatus)
	s.AddTool(tools.TriggerEnrichmentTool(), h.TriggerEnrichment)

	// ── Cache maintenance (cache.db only, never the mailbox) ────────────────
	s.AddTool(tools.CacheSweepTool(), h.CacheSweep)

	// ── Working directory file I/O (enforced output sandbox) ─────────────────
	s.AddTool(tools.WriteFileTool(), h.WriteFile)
	s.AddTool(tools.ReadFileTool(), h.ReadFile)
	s.AddTool(tools.ListFilesTool(), h.ListFiles)
	s.AddTool(tools.DeleteFileTool(), h.DeleteFile)

	return s
}
