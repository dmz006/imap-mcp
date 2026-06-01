package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) MoveMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))
	dest := req.GetString("destination", "")
	if folder == "" || uid == 0 || dest == "" {
		return mcp.NewToolResultError("folder, uid, and destination are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	if _, err := conn.Client().Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	uidSet := imaplib.UIDSetNum(imaplib.UID(uid))
	if _, err := conn.Client().Move(uidSet, dest).Wait(); err != nil {
		// Fallback: copy + delete
		if _, err2 := conn.Client().Copy(uidSet, dest).Wait(); err2 != nil {
			return mcp.NewToolResultError(fmt.Sprintf("move failed: %v", err2)), nil
		}
		storeFlags := &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}
		conn.Client().Store(uidSet, storeFlags, nil).Close() //nolint:errcheck
		conn.Client().Expunge().Close()                       //nolint:errcheck
	}
	return mcp.NewToolResultText(fmt.Sprintf("moved uid=%d from %s to %s", uid, folder, dest)), nil
}

func (h *Handlers) CopyMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))
	dest := req.GetString("destination", "")
	if folder == "" || uid == 0 || dest == "" {
		return mcp.NewToolResultError("folder, uid, and destination are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	if _, err := conn.Client().Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	uidSet := imaplib.UIDSetNum(imaplib.UID(uid))
	if _, err := conn.Client().Copy(uidSet, dest).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("copy: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("copied uid=%d from %s to %s", uid, folder, dest)), nil
}

func (h *Handlers) DeleteMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))
	permanent := req.GetBool("permanent", false)
	if folder == "" || uid == 0 {
		return mcp.NewToolResultError("folder and uid are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	if _, err := conn.Client().Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	uidSet := imaplib.UIDSetNum(imaplib.UID(uid))

	if permanent {
		storeFlags := &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}
		if err := conn.Client().Store(uidSet, storeFlags, nil).Close(); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mark deleted: %v", err)), nil
		}
		if err := conn.Client().Expunge().Close(); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("expunge: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("permanently deleted uid=%d from %s", uid, folder)), nil
	}

	// Try Trash, then [Gmail]/Trash
	trashFolder := "Trash"
	if _, err := conn.Client().Move(uidSet, trashFolder).Wait(); err != nil {
		trashFolder = "[Gmail]/Trash"
		if _, err2 := conn.Client().Move(uidSet, trashFolder).Wait(); err2 != nil {
			storeFlags := &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}
			conn.Client().Store(uidSet, storeFlags, nil).Close() //nolint:errcheck
			return mcp.NewToolResultText(fmt.Sprintf("marked deleted uid=%d (no trash folder found)", uid)), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("moved uid=%d to %s", uid, trashFolder)), nil
}

func (h *Handlers) SetFlags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))
	addFlags := req.GetString("add", "")
	removeFlags := req.GetString("remove", "")
	if folder == "" || uid == 0 {
		return mcp.NewToolResultError("folder and uid are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	if _, err := conn.Client().Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	uidSet := imaplib.UIDSetNum(imaplib.UID(uid))
	if addFlags != "" {
		opts := &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: parseFlags(addFlags)}
		if err := conn.Client().Store(uidSet, opts, nil).Close(); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("add flags: %v", err)), nil
		}
	}
	if removeFlags != "" {
		opts := &imaplib.StoreFlags{Op: imaplib.StoreFlagsDel, Flags: parseFlags(removeFlags)}
		if err := conn.Client().Store(uidSet, opts, nil).Close(); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("remove flags: %v", err)), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("flags updated on uid=%d", uid)), nil
}

func (h *Handlers) AppendMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	message := req.GetString("message", "")
	flagStr := req.GetString("flags", "")
	if folder == "" || message == "" {
		return mcp.NewToolResultError("folder and message are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	appendOpts := &imaplib.AppendOptions{
		Time: time.Now(),
	}
	if flagStr != "" {
		appendOpts.Flags = parseFlags(flagStr)
	}

	msgBytes := []byte(message)
	cmd := conn.Client().Append(folder, int64(len(msgBytes)), appendOpts)
	if _, err := cmd.Write(msgBytes); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write message: %v", err)), nil
	}
	if err := cmd.Close(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append: %v", err)), nil
	}
	data, err := cmd.Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append wait: %v", err)), nil
	}

	uid := uint32(0)
	if data != nil {
		uid = uint32(data.UID)
	}
	return mcp.NewToolResultText(fmt.Sprintf("appended to %s (uid=%d)", folder, uid)), nil
}

func (h *Handlers) MoveBulk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	query := req.GetString("query", "")
	subject := req.GetString("subject", "")
	dest := req.GetString("destination", "")
	limit := int(req.GetFloat("limit", 100))
	if folder == "" || (query == "" && subject == "") || dest == "" {
		return mcp.NewToolResultError("folder, (query or subject), and destination are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	if _, err := conn.Client().Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	criteria := &imaplib.SearchCriteria{}
	if query != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "From", Value: query})
	}
	if subject != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "Subject", Value: subject})
	}

	searchData, err := conn.Client().UIDSearch(criteria, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search: %v", err)), nil
	}

	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return mcp.NewToolResultText("no messages matched"), nil
	}
	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}

	uidSet := imaplib.UIDSetNum(uids...)
	if _, err := conn.Client().Move(uidSet, dest).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("move: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("moved %d messages from %s to %s", len(uids), folder, dest)), nil
}

func (h *Handlers) FlagBulk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	folder := req.GetString("folder", "")
	query := req.GetString("query", "")
	addFlags := req.GetString("add", "")
	removeFlags := req.GetString("remove", "")
	limit := int(req.GetFloat("limit", 100))
	if folder == "" || query == "" {
		return mcp.NewToolResultError("folder and query are required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}
	conn.Lock()
	defer conn.Unlock()

	if _, err := conn.Client().Select(folder, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("select: %v", err)), nil
	}

	criteria := &imaplib.SearchCriteria{
		Header: []imaplib.SearchCriteriaHeaderField{{Key: "From", Value: query}},
	}
	searchData, err := conn.Client().UIDSearch(criteria, nil).Wait()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search: %v", err)), nil
	}

	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return mcp.NewToolResultText("no messages matched"), nil
	}
	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}

	uidSet := imaplib.UIDSetNum(uids...)
	if addFlags != "" {
		opts := &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: parseFlags(addFlags)}
		conn.Client().Store(uidSet, opts, nil).Close() //nolint:errcheck
	}
	if removeFlags != "" {
		opts := &imaplib.StoreFlags{Op: imaplib.StoreFlagsDel, Flags: parseFlags(removeFlags)}
		conn.Client().Store(uidSet, opts, nil).Close() //nolint:errcheck
	}
	return mcp.NewToolResultText(fmt.Sprintf("updated flags on %d messages in %s", len(uids), folder)), nil
}

func parseFlags(s string) []imaplib.Flag {
	var flags []imaplib.Flag
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, "\\") {
			f = "\\" + f
		}
		flags = append(flags, imaplib.Flag(f))
	}
	return flags
}
