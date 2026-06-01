package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

type subscriptionCandidate struct {
	Sender         string   `json:"sender"`
	Domain         string   `json:"domain"`
	SampleSubjects []string `json:"sample_subjects"`
	MessageCount   int      `json:"message_count"`
	UnsubscribeURL string   `json:"unsubscribe_url,omitempty"`
	UnsubMailto    string   `json:"unsubscribe_mailto,omitempty"`
	LatestDate     string   `json:"latest_date"`
	Account        string   `json:"account"`
	Folder         string   `json:"folder"`
}

var listUnsubRe = regexp.MustCompile(`<([^>]+)>`)

func (h *Handlers) DetectSubscriptions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	folder := req.GetString("folder", "INBOX")
	limit := int(req.GetFloat("limit", 500))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}

	accountFilter := req.GetString("account", "")
	accounts := h.pool.AccountNames()
	if accountFilter != "" {
		accounts = []string{accountFilter}
	}

	var allCandidates []*subscriptionCandidate

	for _, acct := range accounts {
		candidates, err := h.scanSubscriptions(ctx, acct, folder, limit)
		if err != nil {
			continue
		}
		allCandidates = append(allCandidates, candidates...)
	}

	// Sort by message count descending
	sort.Slice(allCandidates, func(i, j int) bool {
		return allCandidates[i].MessageCount > allCandidates[j].MessageCount
	})

	result, err := mcp.NewToolResultJSON(map[string]any{
		"total_senders": len(allCandidates),
		"folder":        folder,
		"accounts":      accounts,
		"subscriptions": allCandidates,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

func (h *Handlers) scanSubscriptions(ctx context.Context, account, folder string, limit int) ([]*subscriptionCandidate, error) {
	conn, err := h.pool.Resolve(account)
	if err != nil {
		return nil, err
	}
	conn.Lock()
	defer conn.Unlock()

	client := conn.Client()

	mbox, err := client.Select(folder, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s/%s: %w", account, folder, err)
	}
	if mbox.NumMessages == 0 {
		return nil, nil
	}

	// Fetch the most recent `limit` messages, only LIST-UNSUBSCRIBE + envelope
	end := mbox.NumMessages
	start := uint32(1)
	if int(end) > limit {
		start = end - uint32(limit) + 1
	}

	var seqSet imaplib.SeqSet
	seqSet.AddRange(start, end)

	fetchOpts := &imaplib.FetchOptions{
		UID:          true,
		Envelope:     true,
		InternalDate: true,
		BodySection: []*imaplib.FetchItemBodySection{
			{
				Specifier:    imaplib.PartSpecifierHeader,
				HeaderFields: []string{"LIST-UNSUBSCRIBE"},
			},
		},
	}

	msgs, err := client.Fetch(seqSet, fetchOpts).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	byKey := map[string]*subscriptionCandidate{}

	for _, m := range msgs {
		if m.Envelope == nil {
			continue
		}

		// Extract List-Unsubscribe value from raw header blob
		listUnsub := ""
		for _, bs := range m.BodySection {
			raw := string(bs.Bytes)
			lines := strings.Split(raw, "\n")
			for i, line := range lines {
				line = strings.TrimRight(line, "\r")
				lower := strings.ToLower(line)
				if strings.HasPrefix(lower, "list-unsubscribe:") {
					val := strings.TrimSpace(line[len("list-unsubscribe:"):])
					// Collect continuation lines
					for j := i + 1; j < len(lines); j++ {
						next := lines[j]
						if len(next) > 0 && (next[0] == ' ' || next[0] == '\t') {
							val += " " + strings.TrimSpace(next)
						} else {
							break
						}
					}
					listUnsub = val
					break
				}
			}
		}
		if listUnsub == "" {
			continue
		}

		// Build sender key
		fromAddr := ""
		fromName := ""
		if len(m.Envelope.From) > 0 {
			a := m.Envelope.From[0]
			fromAddr = strings.ToLower(fmt.Sprintf("%s@%s", a.Mailbox, a.Host))
			fromName = a.Name
		}
		if fromAddr == "" {
			continue
		}

		domain := ""
		if parts := strings.Split(fromAddr, "@"); len(parts) == 2 {
			domain = parts[1]
		}

		c, exists := byKey[fromAddr]
		if !exists {
			display := fromAddr
			if fromName != "" {
				display = fmt.Sprintf("%s <%s>", fromName, fromAddr)
			}
			c = &subscriptionCandidate{
				Sender:  display,
				Domain:  domain,
				Account: account,
				Folder:  folder,
			}
			byKey[fromAddr] = c
		}
		c.MessageCount++

		if !m.InternalDate.IsZero() {
			d := m.InternalDate.Format("2006-01-02")
			if d > c.LatestDate {
				c.LatestDate = d
			}
		}

		if len(c.SampleSubjects) < 3 && m.Envelope.Subject != "" {
			subj := strings.TrimSpace(m.Envelope.Subject)
			if !containsStr(c.SampleSubjects, subj) {
				c.SampleSubjects = append(c.SampleSubjects, subj)
			}
		}

		// Extract first unsubscribe URL and mailto (only need one each)
		if c.UnsubscribeURL == "" || c.UnsubMailto == "" {
			for _, match := range listUnsubRe.FindAllStringSubmatch(listUnsub, -1) {
				if len(match) < 2 {
					continue
				}
				val := strings.TrimSpace(match[1])
				if c.UnsubMailto == "" && strings.HasPrefix(strings.ToLower(val), "mailto:") {
					c.UnsubMailto = val
				}
				if c.UnsubscribeURL == "" && strings.HasPrefix(strings.ToLower(val), "http") {
					c.UnsubscribeURL = val
				}
			}
		}
	}

	result := make([]*subscriptionCandidate, 0, len(byKey))
	for _, c := range byKey {
		result = append(result, c)
	}
	return result, nil
}

func (h *Handlers) SummarizeFolder(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	if folder == "" {
		return mcp.NewToolResultError("folder is required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}

	conn.Lock()
	defer conn.Unlock()

	mbox, err := conn.Client().Select(folder, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select %s: %v", folder, err)), nil
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"folder":  folder,
		"account": account,
		"total":   mbox.NumMessages,
		"recent":  mbox.NumRecent,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
