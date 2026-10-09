package tools

import (
	"context"
	"fmt"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/service"
)

func (h *Handlers) MoveMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	folder, uid, dest := req.GetString("folder", ""), uint32(req.GetFloat("uid", 0)), req.GetString("destination", "")
	err := h.svc.MoveMessage(ctx, req.GetString("account", ""), folder, uid, dest)
	return text(fmt.Sprintf("moved uid=%d from %s to %s", uid, folder, dest), err)
}

func (h *Handlers) CopyMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	folder, uid, dest := req.GetString("folder", ""), uint32(req.GetFloat("uid", 0)), req.GetString("destination", "")
	err := h.svc.CopyMessage(ctx, req.GetString("account", ""), folder, uid, dest)
	return text(fmt.Sprintf("copied uid=%d from %s to %s", uid, folder, dest), err)
}

func (h *Handlers) DeleteMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	folder, uid := req.GetString("folder", ""), uint32(req.GetFloat("uid", 0))
	res, err := h.svc.DeleteMessage(ctx, req.GetString("account", ""), folder, uid, req.GetBool("permanent", false))
	switch {
	case err != nil:
		return text("", err)
	case res.Permanent:
		return text(fmt.Sprintf("permanently deleted uid=%d from %s", uid, folder), nil)
	case res.Trash != "":
		return text(fmt.Sprintf("moved uid=%d to %s", uid, res.Trash), nil)
	}
	return text(fmt.Sprintf("marked deleted uid=%d (no trash folder found)", uid), nil)
}

func (h *Handlers) SetFlags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := uint32(req.GetFloat("uid", 0))
	err := h.svc.SetFlags(ctx, req.GetString("account", ""), req.GetString("folder", ""), uid, req.GetString("add", ""), req.GetString("remove", ""))
	return text(fmt.Sprintf("flags updated on uid=%d", uid), err)
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
		appendOpts.Flags = service.ParseFlags(flagStr)
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
	if err := service.MoveUIDs(conn.Client(), uidSet, dest); err != nil {
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
		opts := &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: service.ParseFlags(addFlags)}
		conn.Client().Store(uidSet, opts, nil).Close() //nolint:errcheck
	}
	if removeFlags != "" {
		opts := &imaplib.StoreFlags{Op: imaplib.StoreFlagsDel, Flags: service.ParseFlags(removeFlags)}
		conn.Client().Store(uidSet, opts, nil).Close() //nolint:errcheck
	}
	return mcp.NewToolResultText(fmt.Sprintf("updated flags on %d messages in %s", len(uids), folder)), nil
}
