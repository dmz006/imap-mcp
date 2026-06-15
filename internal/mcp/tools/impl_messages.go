package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mark3labs/mcp-go/mcp"
)

// messageHeader is the JSON-friendly representation of a message header.
type messageHeader struct {
	UID      uint32   `json:"uid"`
	SeqNum   uint32   `json:"seq_num"`
	Subject  string   `json:"subject"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Date     string   `json:"date"`
	Flags    []string `json:"flags"`
	Size     int64    `json:"size_bytes"`
	ThreadID string   `json:"thread_id,omitempty"`
}

type messageDetail struct {
	messageHeader
	BodyText string `json:"body_text,omitempty"`
	BodyHTML string `json:"body_html,omitempty"`
}

func (h *Handlers) ListMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "INBOX")
	limit := int(req.GetFloat("limit", 50))
	offset := int(req.GetFloat("offset", 0))
	order := req.GetString("order", "desc")

	if limit <= 0 || limit > 200 {
		limit = 50
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
		return mcp.NewToolResultJSON([]messageHeader{})
	}

	// Build sequence set for pagination. Seq 1 = oldest, seq N = newest.
	// desc order: most recent first. Clamp to valid range.
	var seqSet imaplib.SeqSet
	if order == "asc" {
		start := uint32(offset + 1)
		end := uint32(offset + limit)
		if start > total {
			return mcp.NewToolResultJSON([]messageHeader{})
		}
		if end > total {
			end = total
		}
		seqSet.AddRange(start, end)
	} else {
		// desc: newest first
		if uint32(offset) >= total {
			return mcp.NewToolResultJSON([]messageHeader{})
		}
		end := total - uint32(offset)
		start := uint32(1)
		if int(end) > limit {
			start = end - uint32(limit) + 1
		}
		seqSet.AddRange(start, end)
	}

	fetchOpts := &imaplib.FetchOptions{
		Envelope:     true,
		Flags:        true,
		UID:          true,
		RFC822Size:   true,
		InternalDate: true,
	}

	msgs, err := client.Fetch(seqSet, fetchOpts).Collect()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch: %v", err)), nil
	}

	headers := make([]messageHeader, 0, len(msgs))
	for _, m := range msgs {
		hdr := msgToHeader(m)
		headers = append(headers, hdr)
	}

	// Reverse for desc order so newest is first
	if order != "asc" {
		for i, j := 0, len(headers)-1; i < j; i, j = i+1, j-1 {
			headers[i], headers[j] = headers[j], headers[i]
		}
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"folder":  folder,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
		"count":   len(headers),
		"messages": headers,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

func (h *Handlers) GetMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))

	if folder == "" {
		return mcp.NewToolResultError("folder is required"), nil
	}
	if uid == 0 {
		return mcp.NewToolResultError("uid is required"), nil
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

	// Fetch by UID
	uidSet := imaplib.UIDSetNum(imaplib.UID(uid))
	fetchOpts := &imaplib.FetchOptions{
		Envelope:     true,
		Flags:        true,
		UID:          true,
		RFC822Size:   true,
		InternalDate: true,
		BodySection: []*imaplib.FetchItemBodySection{
			{Specifier: imaplib.PartSpecifierText},
		},
	}

	msgs, err := client.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("uid fetch: %v", err)), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("message uid=%d not found in %s", uid, folder)), nil
	}

	m := msgs[0]
	hdr := msgToHeader(m)
	detail := messageDetail{messageHeader: hdr}

	// Extract text body
	for _, bs := range m.BodySection {
		if bs.Section != nil && bs.Section.Specifier == imaplib.PartSpecifierText {
			detail.BodyText = string(bs.Bytes)
		}
	}

	result, err := mcp.NewToolResultJSON(detail)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

func (h *Handlers) GetHeaders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))

	if folder == "" || uid == 0 {
		return mcp.NewToolResultError("folder and uid are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}

	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	uidSet := imaplib.UIDSetNum(imaplib.UID(uid))
	fetchOpts := &imaplib.FetchOptions{
		Envelope:     true,
		Flags:        true,
		UID:          true,
		RFC822Size:   true,
		InternalDate: true,
		BodySection: []*imaplib.FetchItemBodySection{
			{Specifier: imaplib.PartSpecifierHeader},
		},
	}

	msgs, err := client.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch: %v", err)), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError("message not found"), nil
	}

	m := msgs[0]
	hdr := msgToHeader(m)

	var rawHeaders string
	for _, bs := range m.BodySection {
		if bs.Section != nil && bs.Section.Specifier == imaplib.PartSpecifierHeader {
			rawHeaders = string(bs.Bytes)
		}
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"uid":         hdr.UID,
		"subject":     hdr.Subject,
		"from":        hdr.From,
		"to":          hdr.To,
		"date":        hdr.Date,
		"flags":       hdr.Flags,
		"size_bytes":  hdr.Size,
		"raw_headers": rawHeaders,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

func (h *Handlers) SearchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "INBOX")
	fromFilter := req.GetString("from", "")
	subjectFilter := req.GetString("subject", "")
	textFilter := req.GetString("text", "")
	sinceStr := req.GetString("since", "")
	beforeStr := req.GetString("before", "")
	flagFilter := req.GetString("flags", "")
	limit := int(req.GetFloat("limit", 50))

	if limit <= 0 || limit > 500 {
		limit = 50
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

	criteria := &imaplib.SearchCriteria{}

	if fromFilter != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{
			Key:   "From",
			Value: fromFilter,
		})
	}
	if subjectFilter != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{
			Key:   "Subject",
			Value: subjectFilter,
		})
	}
	if textFilter != "" {
		criteria.Body = append(criteria.Body, textFilter)
	}
	if sinceStr != "" {
		if t, err := time.Parse("2006-01-02", sinceStr); err == nil {
			criteria.Since = t
		}
	}
	if beforeStr != "" {
		if t, err := time.Parse("2006-01-02", beforeStr); err == nil {
			criteria.Before = t
		}
	}
	switch strings.ToLower(flagFilter) {
	case "seen":
		criteria.Flag = append(criteria.Flag, imaplib.FlagSeen)
	case "unseen":
		criteria.NotFlag = append(criteria.NotFlag, imaplib.FlagSeen)
	case "flagged":
		criteria.Flag = append(criteria.Flag, imaplib.FlagFlagged)
	case "answered":
		criteria.Flag = append(criteria.Flag, imaplib.FlagAnswered)
	}

	searchData, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search: %v", err)), nil
	}

	uids := searchData.AllUIDs()
	// Capture the TRUE total before truncating to the page limit. IMAP UIDSearch
	// returns every matching UID, so this is exact — not capped at `limit`.
	totalMatches := len(uids)
	if len(uids) == 0 {
		result, _ := mcp.NewToolResultJSON(map[string]any{
			"folder": folder, "total_matches": 0, "returned": 0, "messages": []messageHeader{},
		})
		return result, nil
	}

	// Cap to limit (take most recent = highest UIDs)
	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}

	uidSet := imaplib.UIDSetNum(uids...)
	fetchOpts := &imaplib.FetchOptions{
		Envelope: true, Flags: true, UID: true, RFC822Size: true, InternalDate: true,
	}

	msgs, err := client.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch results: %v", err)), nil
	}

	headers := make([]messageHeader, 0, len(msgs))
	for _, m := range msgs {
		headers = append(headers, msgToHeader(m))
	}
	// Reverse for newest-first
	for i, j := 0, len(headers)-1; i < j; i, j = i+1, j-1 {
		headers[i], headers[j] = headers[j], headers[i]
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"folder":        folder,
		"total_matches": totalMatches,
		"returned":      len(headers),
		"messages":      headers,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func msgToHeader(m *imapclient.FetchMessageBuffer) messageHeader {
	hdr := messageHeader{
		UID:    uint32(m.UID),
		SeqNum: m.SeqNum,
		Size:   m.RFC822Size,
		Flags:  flagStrings(m.Flags),
	}
	if !m.InternalDate.IsZero() {
		hdr.Date = m.InternalDate.Format(time.RFC3339)
	}
	if env := m.Envelope; env != nil {
		hdr.Subject = env.Subject
		if len(env.From) > 0 {
			a := env.From[0]
			if a.Name != "" {
				hdr.From = fmt.Sprintf("%s <%s@%s>", a.Name, a.Mailbox, a.Host)
			} else {
				hdr.From = fmt.Sprintf("%s@%s", a.Mailbox, a.Host)
			}
		}
		for _, a := range env.To {
			hdr.To = append(hdr.To, fmt.Sprintf("%s@%s", a.Mailbox, a.Host))
		}
		if len(env.InReplyTo) > 0 {
			hdr.ThreadID = strings.Join(env.InReplyTo, " ")
		}
	}
	return hdr
}

func flagStrings(flags []imaplib.Flag) []string {
	s := make([]string, len(flags))
	for i, f := range flags {
		s[i] = string(f)
	}
	return s
}
