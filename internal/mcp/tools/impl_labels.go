package tools

import (
	"context"
	"fmt"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

// LabelMessage applies a Gmail label by COPYing the message into the label's
// mailbox (the message keeps its current location and gains the label). This is
// the standard-IMAP way Gmail labels work; the label mailbox is created if it
// does not exist.
func (h *Handlers) LabelMessage(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "INBOX")
	uid := uint32(req.GetFloat("uid", 0))
	label := req.GetString("label", "")
	if uid == 0 || label == "" {
		return mcp.NewToolResultError("uid and label are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select %s: %v", folder, err)), nil
	}

	// Ensure the label mailbox exists (ignore "already exists").
	if req.GetBool("create", true) {
		_ = client.Create(label, nil).Wait()
	}

	set := imaplib.UIDSetNum(imaplib.UID(uid))
	if _, err := client.Copy(set, label).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("apply label %q: %v", label, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("labeled uid=%d in %q with %q", uid, folder, label)), nil
}

// LabelBulk applies a Gmail label to every message matching a sender (and
// optional subject) by COPYing them into the label mailbox in one call. The
// originals stay in place (Gmail: the message simply gains the label). The
// label is created if missing. Unlike a move, this does NOT loop — COPY leaves
// messages where they are, so one pass labels the whole match set.
func (h *Handlers) LabelBulk(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "INBOX")
	from := req.GetString("from", "")
	subject := req.GetString("subject", "")
	label := req.GetString("label", "")
	if label == "" || (from == "" && subject == "") {
		return mcp.NewToolResultError("label and (from or subject) are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select %s: %v", folder, err)), nil
	}
	if req.GetBool("create", true) {
		_ = client.Create(label, nil).Wait() // ignore "already exists"
	}

	criteria := &imaplib.SearchCriteria{}
	if from != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "From", Value: from})
	}
	if subject != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "Subject", Value: subject})
	}
	sd, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search: %v", err)), nil
	}
	uids := sd.AllUIDs()
	if len(uids) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("no messages matched (label %q not applied)", label)), nil
	}
	set := imaplib.UIDSetNum(uids...)
	if _, err := client.Copy(set, label).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("apply label %q: %v", label, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("labeled %d messages with %q", len(uids), label)), nil
}

// EmptyTrash permanently deletes everything in the account's Trash mailbox.
func (h *Handlers) EmptyTrash(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	trash, err := resolveTrash(client)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("find trash: %v", err)), nil
	}
	mbox, err := client.Select(trash, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select %s: %v", trash, err)), nil
	}
	if mbox.NumMessages == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("%s already empty", trash)), nil
	}

	var all imaplib.SeqSet
	all.AddRange(1, mbox.NumMessages)
	if err := client.Store(all, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("flag deleted: %v", err)), nil
	}
	if err := client.Expunge().Close(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("expunge: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("emptied %s (%d messages permanently deleted)", trash, mbox.NumMessages)), nil
}
