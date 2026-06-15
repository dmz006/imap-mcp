package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

// TopSenders ranks senders in a folder by message count, so bulk/spam clusters
// surface in one call instead of client-side pagination + tally.
func (h *Handlers) TopSenders(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "INBOX")
	topN := int(req.GetFloat("top", 30))
	scan := int(req.GetFloat("scan", 0)) // 0 = all
	groupBy := strings.ToLower(req.GetString("group_by", "address"))
	if topN <= 0 {
		topN = 30
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	mbox, err := client.Select(folder, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select %s: %v", folder, err)), nil
	}
	total := mbox.NumMessages
	if total == 0 {
		result, _ := mcp.NewToolResultJSON(map[string]any{"folder": folder, "scanned": 0, "senders": []any{}})
		return result, nil
	}

	// Choose the window: most recent `scan` messages, or all.
	start, end := uint32(1), total
	if scan > 0 && uint32(scan) < total {
		start = total - uint32(scan) + 1
	}
	var seqSet imaplib.SeqSet
	seqSet.AddRange(start, end)

	msgs, err := client.Fetch(seqSet, &imaplib.FetchOptions{Envelope: true}).Collect()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch envelopes: %v", err)), nil
	}

	counts := map[string]int{}
	names := map[string]string{} // representative display name per key
	for _, m := range msgs {
		if m.Envelope == nil || len(m.Envelope.From) == 0 {
			continue
		}
		f := m.Envelope.From[0]
		addr := strings.ToLower(f.Addr())
		if addr == "@" || addr == "" {
			continue
		}
		key := addr
		if groupBy == "domain" {
			if at := strings.LastIndex(addr, "@"); at >= 0 {
				key = addr[at+1:]
			}
		}
		counts[key]++
		if names[key] == "" && f.Name != "" {
			names[key] = f.Name
		}
	}

	type senderStat struct {
		Key   string `json:"sender"`
		Name  string `json:"name,omitempty"`
		Count int    `json:"count"`
	}
	stats := make([]senderStat, 0, len(counts))
	for k, c := range counts {
		stats = append(stats, senderStat{Key: k, Name: names[k], Count: c})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Count != stats[j].Count {
			return stats[i].Count > stats[j].Count
		}
		return stats[i].Key < stats[j].Key
	})
	if len(stats) > topN {
		stats = stats[:topN]
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"folder":         folder,
		"folder_total":   total,
		"scanned":        len(msgs),
		"unique_senders": len(counts),
		"group_by":       groupBy,
		"senders":        stats,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}
