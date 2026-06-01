package tools

import (
	"github.com/mark3labs/mcp-go/mcp"
)

// ── Account ──────────────────────────────────────────────────────────────────

func ListAccountsTool() mcp.Tool {
	return mcp.NewTool("list_accounts",
		mcp.WithDescription("List all configured IMAP accounts and their connection status"),
	)
}

func SyncAccountTool() mcp.Tool {
	return mcp.NewTool("sync_account",
		mcp.WithDescription("Trigger an immediate IMAP sync for an account"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
	)
}

// ── Folders ──────────────────────────────────────────────────────────────────

func ListFoldersTool() mcp.Tool {
	return mcp.NewTool("list_folders",
		mcp.WithDescription("List all mailbox folders/labels for an account"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
	)
}

func CreateFolderTool() mcp.Tool {
	return mcp.NewTool("create_folder",
		mcp.WithDescription("Create a new mailbox folder"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("path", mcp.Required(), mcp.Description("Folder path, e.g. Archive/2026")),
	)
}

func DeleteFolderTool() mcp.Tool {
	return mcp.NewTool("delete_folder",
		mcp.WithDescription("Delete a mailbox folder (must be empty)"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("path", mcp.Required(), mcp.Description("Folder path to delete")),
	)
}

// ── Messages (read) ──────────────────────────────────────────────────────────

func ListMessagesTool() mcp.Tool {
	return mcp.NewTool("list_messages",
		mcp.WithDescription("List messages in a folder with headers. Paginated."),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Description("Folder path (default: INBOX)")),
		mcp.WithNumber("limit", mcp.Description("Max messages to return (default: 50)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
		mcp.WithString("sort", mcp.Description("Sort field: date|subject|from (default: date)")),
		mcp.WithString("order", mcp.Description("asc|desc (default: desc)")),
	)
}

func GetMessageTool() mcp.Tool {
	return mcp.NewTool("get_message",
		mcp.WithDescription("Fetch a full message by UID including headers, body, and attachment metadata"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder containing the message")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
	)
}

func GetThreadTool() mcp.Tool {
	return mcp.NewTool("get_thread",
		mcp.WithDescription("Fetch all messages in a conversation thread"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("Thread ID from list_messages")),
	)
}

func GetHeadersTool() mcp.Tool {
	return mcp.NewTool("get_headers",
		mcp.WithDescription("Fetch raw headers only (fast, no body). Useful for routing/filtering logic."),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder containing the message")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
	)
}

func GetAttachmentsTool() mcp.Tool {
	return mcp.NewTool("get_attachments",
		mcp.WithDescription("List or download attachments from a message"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder containing the message")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
		mcp.WithString("part", mcp.Description("Attachment part ID to download (omit to list all)")),
	)
}

func ExportMessageTool() mcp.Tool {
	return mcp.NewTool("export_message",
		mcp.WithDescription("Export a message in EML format for use with other tools"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder containing the message")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
	)
}

// ── Messages (write) ─────────────────────────────────────────────────────────

func MoveMessageTool() mcp.Tool {
	return mcp.NewTool("move_message",
		mcp.WithDescription("Move a message to another folder"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Source folder")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
		mcp.WithString("destination", mcp.Required(), mcp.Description("Destination folder path")),
	)
}

func CopyMessageTool() mcp.Tool {
	return mcp.NewTool("copy_message",
		mcp.WithDescription("Copy a message to another folder"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Source folder")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
		mcp.WithString("destination", mcp.Required(), mcp.Description("Destination folder path")),
	)
}

func DeleteMessageTool() mcp.Tool {
	return mcp.NewTool("delete_message",
		mcp.WithDescription("Delete a message (moves to Trash or expunges if already in Trash)"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder containing the message")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
		mcp.WithBoolean("permanent", mcp.Description("Expunge immediately without moving to Trash")),
	)
}

func SetFlagsTool() mcp.Tool {
	return mcp.NewTool("set_flags",
		mcp.WithDescription("Set or clear flags on a message (Seen, Flagged, Answered, etc.)"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder containing the message")),
		mcp.WithNumber("uid", mcp.Required(), mcp.Description("Message UID")),
		mcp.WithString("add", mcp.Description("Comma-separated flags to add: Seen,Flagged,Answered")),
		mcp.WithString("remove", mcp.Description("Comma-separated flags to remove")),
	)
}

func AppendMessageTool() mcp.Tool {
	return mcp.NewTool("append_message",
		mcp.WithDescription("Write a raw RFC 2822 message into a folder (archiving, importing)"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Destination folder")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Raw RFC 2822 message content")),
		mcp.WithString("flags", mcp.Description("Comma-separated initial flags: Seen,Flagged")),
	)
}

func MoveBulkTool() mcp.Tool {
	return mcp.NewTool("move_bulk",
		mcp.WithDescription("Move all messages matching a search query to a destination folder"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Source folder")),
		mcp.WithString("query", mcp.Required(), mcp.Description("IMAP SEARCH criteria or FTS query")),
		mcp.WithString("destination", mcp.Required(), mcp.Description("Destination folder path")),
		mcp.WithNumber("limit", mcp.Description("Max messages to move (safety cap, default: 100)")),
	)
}

func FlagBulkTool() mcp.Tool {
	return mcp.NewTool("flag_bulk",
		mcp.WithDescription("Set flags on all messages matching a search query"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder to search")),
		mcp.WithString("query", mcp.Required(), mcp.Description("IMAP SEARCH criteria or FTS query")),
		mcp.WithString("add", mcp.Description("Comma-separated flags to add")),
		mcp.WithString("remove", mcp.Description("Comma-separated flags to remove")),
		mcp.WithNumber("limit", mcp.Description("Max messages to flag (safety cap, default: 100)")),
	)
}

// ── Search ───────────────────────────────────────────────────────────────────

func SearchMessagesTool() mcp.Tool {
	return mcp.NewTool("search_messages",
		mcp.WithDescription("Search messages using IMAP SEARCH, FTS full-text, or enrichment tags"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Description("Folder to search (omit for all synced folders)")),
		mcp.WithString("from", mcp.Description("Filter by sender address or name")),
		mcp.WithString("subject", mcp.Description("Subject keyword")),
		mcp.WithString("text", mcp.Description("Full-text search in subject+body")),
		mcp.WithString("since", mcp.Description("ISO 8601 date, e.g. 2026-01-01")),
		mcp.WithString("before", mcp.Description("ISO 8601 date")),
		mcp.WithString("flags", mcp.Description("Filter by flags: Seen|Unseen|Flagged|Answered")),
		mcp.WithString("hall", mcp.Description("Filter by hall tag: conversation|newsletter|transactional|notification|alert|personal")),
		mcp.WithString("wing", mcp.Description("Filter by wing (project) tag")),
		mcp.WithString("room", mcp.Description("Filter by room (topic) tag")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 50)")),
	)
}

func CrossAccountSearchTool() mcp.Tool {
	return mcp.NewTool("cross_account_search",
		mcp.WithDescription("Search across all connected accounts simultaneously"),
		mcp.WithString("from", mcp.Description("Filter by sender")),
		mcp.WithString("subject", mcp.Description("Subject keyword")),
		mcp.WithString("text", mcp.Description("Full-text search")),
		mcp.WithString("since", mcp.Description("ISO 8601 date")),
		mcp.WithString("before", mcp.Description("ISO 8601 date")),
		mcp.WithNumber("limit", mcp.Description("Max results per account (default: 20)")),
	)
}

func SemanticSearchTool() mcp.Tool {
	return mcp.NewTool("semantic_search",
		mcp.WithDescription("Find messages semantically similar to a query or reference message using vector embeddings"),
		mcp.WithString("account", mcp.Description("Account name (omit for all accounts)")),
		mcp.WithString("query", mcp.Description("Natural language query")),
		mcp.WithNumber("reference_uid", mcp.Description("UID of a reference message to find similar messages")),
		mcp.WithString("folder", mcp.Description("Folder to search (omit for all)")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 10)")),
		mcp.WithNumber("threshold", mcp.Description("Minimum similarity 0.0-1.0 (default: 0.7)")),
	)
}

// ── Intelligence ─────────────────────────────────────────────────────────────

func SummarizeFolderTool() mcp.Tool {
	return mcp.NewTool("summarize_folder",
		mcp.WithDescription("Get folder statistics without reading every message: count, unread, top senders, size, hall breakdown, recent anomalies"),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Required(), mcp.Description("Folder path")),
	)
}

func DetectSubscriptionsTool() mcp.Tool {
	return mcp.NewTool("detect_subscriptions",
		mcp.WithDescription("Scan for emails with List-Unsubscribe headers. Returns subscription candidates with unsubscribe links for inbox cleanup."),
		mcp.WithString("account", mcp.Description("Account name (omit for default)")),
		mcp.WithString("folder", mcp.Description("Folder to scan (default: INBOX)")),
		mcp.WithNumber("limit", mcp.Description("Max candidates to return (default: 50)")),
	)
}

func GetSenderHistoryTool() mcp.Tool {
	return mcp.NewTool("get_sender_history",
		mcp.WithDescription("Get all messages from a specific sender address across all folders"),
		mcp.WithString("account", mcp.Description("Account name (omit for all accounts)")),
		mcp.WithString("address", mcp.Required(), mcp.Description("Sender email address")),
		mcp.WithNumber("limit", mcp.Description("Max messages (default: 100)")),
	)
}

func GetSenderProfileTool() mcp.Tool {
	return mcp.NewTool("get_sender_profile",
		mcp.WithDescription("Get enriched sender profile: role, first/last contact, message count, avg reply time, KG relationships, anomalies"),
		mcp.WithString("address", mcp.Required(), mcp.Description("Sender email address")),
	)
}

func KGQueryTool() mcp.Tool {
	return mcp.NewTool("kg_query",
		mcp.WithDescription("Query the temporal knowledge graph for entity relationships"),
		mcp.WithString("entity", mcp.Description("Entity name or email address")),
		mcp.WithString("predicate", mcp.Description("Relationship type to filter (e.g. manages, belongs_to, is_subscription)")),
		mcp.WithString("entity_type", mcp.Description("Entity type: person|organization|topic|project|thread")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 50)")),
	)
}

func GetAnomaliesTool() mcp.Tool {
	return mcp.NewTool("get_anomalies",
		mcp.WithDescription("Get detected anomalies: behavior changes, silence, reply spikes, new senders"),
		mcp.WithString("account", mcp.Description("Account name (omit for all)")),
		mcp.WithString("severity", mcp.Description("Filter by severity: low|medium|high")),
		mcp.WithBoolean("unresolved_only", mcp.Description("Only return unresolved anomalies (default: true)")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
	)
}

func EnrichmentStatusTool() mcp.Tool {
	return mcp.NewTool("enrichment_status",
		mcp.WithDescription("Get enrichment pipeline status: pending, processing, done, error counts per account"),
		mcp.WithString("account", mcp.Description("Account name (omit for all)")),
	)
}

func TriggerEnrichmentTool() mcp.Tool {
	return mcp.NewTool("trigger_enrichment",
		mcp.WithDescription("Force immediate enrichment processing for pending messages"),
		mcp.WithString("account", mcp.Description("Account name (omit for all)")),
		mcp.WithNumber("limit", mcp.Description("Max messages to enrich in this run (default: 50)")),
	)
}

// ── Working directory file I/O ────────────────────────────────────────────────
// All file output must go through these tools. The working_dir config value
// is the only permitted write destination — paths are enforced server-side.

func WriteFileTool() mcp.Tool {
	return mcp.NewTool("write_file",
		mcp.WithDescription("Write content to a file in the configured working directory. This is the ONLY way to save output to disk — all file writes are enforced to stay within working_dir. Use a relative filename; subdirectories are created automatically."),
		mcp.WithString("filename", mcp.Required(), mcp.Description("Relative filename, e.g. 'subscriptions.md' or 'reports/inbox-summary.txt'. No absolute paths, no '..' traversal.")),
		mcp.WithString("content", mcp.Required(), mcp.Description("File content to write")),
	)
}

func ReadFileTool() mcp.Tool {
	return mcp.NewTool("read_file",
		mcp.WithDescription("Read a file from the working directory"),
		mcp.WithString("filename", mcp.Required(), mcp.Description("Relative filename within the working directory")),
	)
}

func ListFilesTool() mcp.Tool {
	return mcp.NewTool("list_files",
		mcp.WithDescription("List files in the working directory or a subdirectory within it"),
		mcp.WithString("subdir", mcp.Description("Subdirectory to list (omit for root of working dir)")),
	)
}

func DeleteFileTool() mcp.Tool {
	return mcp.NewTool("delete_file",
		mcp.WithDescription("Delete a file from the working directory"),
		mcp.WithString("filename", mcp.Required(), mcp.Description("Relative filename to delete within the working directory")),
	)
}
