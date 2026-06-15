package tools

import (
	"context"
	"fmt"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mark3labs/mcp-go/mcp"
)

// PurgeSender moves every message from a given sender to Trash, looping until the
// folder is drained (no 500-cap sweep dance). It auto-detects the account's Trash
// mailbox (Gmail's `[Gmail]/Trash` vs a plain `Trash`). With permanent=true it
// expunges in place instead of moving to Trash.
func (h *Handlers) PurgeSender(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	from := req.GetString("from", "")
	folder := req.GetString("folder", "INBOX")
	permanent := req.GetBool("permanent", false)
	if from == "" {
		return mcp.NewToolResultError("from is required (sender substring to purge)"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	trash := ""
	if !permanent {
		trash, err = resolveTrash(client)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not find Trash folder: %v", err)), nil
		}
	}

	criteria := &imaplib.SearchCriteria{
		Header: []imaplib.SearchCriteriaHeaderField{{Key: "From", Value: from}},
	}

	total := 0
	const maxRounds = 200 // safety backstop (200 * server batch)
	for round := 0; round < maxRounds; round++ {
		if _, err := client.Select(folder, nil).Wait(); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("select %s: %v", folder, err)), nil
		}
		sd, err := client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search: %v", err)), nil
		}
		uids := sd.AllUIDs()
		if len(uids) == 0 {
			break
		}
		set := imaplib.UIDSetNum(uids...)
		if permanent {
			if err := client.Store(set, &imaplib.StoreFlags{
				Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted},
			}, nil).Close(); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("flag deleted: %v", err)), nil
			}
			if err := client.Expunge().Close(); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("expunge: %v", err)), nil
			}
		} else {
			if _, err := client.Move(set, trash).Wait(); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("move to %s: %v", trash, err)), nil
			}
		}
		total += len(uids)
	}

	dest := trash
	if permanent {
		dest = "(expunged)"
	}
	return mcp.NewToolResultText(fmt.Sprintf("purged %d messages from %q matching from=%q → %s", total, folder, from, dest)), nil
}

// resolveTrash finds the account's Trash mailbox, preferring the IMAP \Trash
// special-use attribute, then common names.
func resolveTrash(client *imapclient.Client) (string, error) {
	boxes, err := client.List("", "*", &imaplib.ListOptions{
		ReturnSpecialUse: true,
	}).Collect()
	if err != nil {
		return "", err
	}
	var nameMatch string
	for _, b := range boxes {
		for _, attr := range b.Attrs {
			if attr == imaplib.MailboxAttrTrash {
				return b.Mailbox, nil
			}
		}
		n := b.Mailbox
		if n == "[Gmail]/Trash" || n == "Trash" || strings.HasSuffix(n, "/Trash") {
			nameMatch = n
		}
	}
	if nameMatch != "" {
		return nameMatch, nil
	}
	return "", fmt.Errorf("no \\Trash mailbox found")
}
