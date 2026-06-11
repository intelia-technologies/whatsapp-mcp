package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"whatsapp-mcp/storage"

	"github.com/mark3labs/mcp-go/mcp"
)

// getDisplayName returns the best available name for a chat
// Priority: ContactName > PushName > JID
func getDisplayName(chat storage.Chat) string {
	if chat.ContactName != "" {
		return chat.ContactName
	}
	if chat.PushName != "" {
		return chat.PushName
	}
	return chat.JID
}

// getSenderDisplayName returns the best available name for a message sender
// Priority: ContactName > PushName > JID
func getSenderDisplayName(msg storage.MessageWithNames) string {
	if msg.SenderContactName != "" {
		return msg.SenderContactName
	}
	if msg.SenderPushName != "" {
		return msg.SenderPushName
	}
	return msg.SenderJID
}

// toLocalTime converts a UTC timestamp to the configured timezone.
func (m *MCPServer) toLocalTime(t time.Time) time.Time {
	return t.In(m.timezone)
}

// formatDateTime formats a timestamp in the configured timezone for date and time display.
func (m *MCPServer) formatDateTime(t time.Time) string {
	return m.toLocalTime(t).Format("2006-01-02 15:04:05")
}

// formatTime formats a timestamp in the configured timezone for time-only display.
func (m *MCPServer) formatTime(t time.Time) string {
	return m.toLocalTime(t).Format("15:04:05")
}

// parseTimestamp converts an ISO 8601 timestamp string to time.Time in the server's timezone.
// It supports the formats: "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02".
func (m *MCPServer) parseTimestamp(timestampStr string) (time.Time, error) {
	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}

	for _, format := range formats {
		if t, err := time.ParseInLocation(format, timestampStr, m.timezone); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("invalid timestamp format: %s (expected ISO 8601 like '2006-01-02T15:04:05' or '2006-01-02')", timestampStr)
}

// detectPatternType determines whether a search query should use GLOB matching.
// It returns true if the query contains glob wildcards: * ? [
func detectPatternType(query string) bool {
	return strings.ContainsAny(query, "*?[")
}

// formatFileSize converts bytes to a human-readable size string.
func formatFileSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	if bytes >= GB {
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	} else if bytes >= MB {
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	} else if bytes >= KB {
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	}
	return fmt.Sprintf("%d B", bytes)
}

// formatDimensions returns a formatted dimensions string from width and height.
func formatDimensions(width, height *int) string {
	if width != nil && height != nil {
		return fmt.Sprintf("%dx%d", *width, *height)
	}
	return ""
}

// formatDuration converts seconds to MM:SS format.
func formatDuration(seconds *int) string {
	if seconds == nil {
		return ""
	}
	s := *seconds
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// handleListChats handles the list_chats tool request.
func (m *MCPServer) handleListChats(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// get limit parameter with default
	limit := request.GetFloat("limit", 50.0)
	if limit > 100 {
		limit = 100
	}

	// query database
	chats, err := m.store.ListChats(int(limit))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list chats: %v", err)), nil
	}

	// format response
	var result strings.Builder
	fmt.Fprintf(&result, "Found %d chats:\n\n", len(chats))

	for i, chat := range chats {
		chatType := "DM"
		if chat.IsGroup {
			chatType = "Group"
		}

		jid := chat.JID
		displayName := getDisplayName(chat)
		fmt.Fprintf(&result, "%d. [%s] %s\n", i+1, chatType, displayName)
		fmt.Fprintf(&result, "   JID: %s\n", jid)
		if chat.ContactName != "" && chat.PushName != "" && chat.ContactName != chat.PushName {
			fmt.Fprintf(&result, "   (Contact: %s, Push: %s)\n", chat.ContactName, chat.PushName)
		}
		fmt.Fprintf(&result, "   Last message: %s\n", m.formatDateTime(chat.LastMessageTime))
		if chat.UnreadCount > 0 {
			fmt.Fprintf(&result, "   Unread: %d\n", chat.UnreadCount)
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleGetChatMessages handles the get_chat_messages tool request.
func (m *MCPServer) handleGetChatMessages(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// get required chat_jid
	chatJID, err := request.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError("chat_jid parameter is required"), nil
	}

	// get optional limit
	limit := request.GetFloat("limit", 50.0)
	if limit > 200 {
		limit = 200
	}

	// get optional timestamp filters
	var beforeTime *time.Time
	var afterTime *time.Time

	beforeStr := request.GetString("before_timestamp", "")
	if beforeStr != "" {
		t, err := m.parseTimestamp(beforeStr)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid before_timestamp: %v", err)), nil
		}
		beforeTime = &t
	}

	afterStr := request.GetString("after_timestamp", "")
	if afterStr != "" {
		t, err := m.parseTimestamp(afterStr)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid after_timestamp: %v", err)), nil
		}
		afterTime = &t
	}

	// get optional sender filter
	senderJID := request.GetString("from", "")

	// query database
	var messages []storage.MessageWithNames

	if beforeTime != nil || afterTime != nil || senderJID != "" {
		// use new filtered method
		messages, err = m.store.GetChatMessagesWithNamesFiltered(
			chatJID,
			int(limit),
			beforeTime,
			afterTime,
			senderJID,
		)
	} else {
		// backward compatibility: use offset if no timestamp filters
		offset := request.GetFloat("offset", 0.0)
		messages, err = m.store.GetChatMessagesWithNames(chatJID, int(limit), int(offset))
	}

	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get messages: %v", err)), nil
	}

	// format response
	var result strings.Builder
	fmt.Fprintf(&result, "Retrieved %d messages from chat %s", len(messages), chatJID)

	if senderJID != "" {
		fmt.Fprintf(&result, " (filtered by sender: %s)", senderJID)
	}
	if beforeTime != nil {
		fmt.Fprintf(&result, " (before: %s)", m.formatDateTime(*beforeTime))
	}
	if afterTime != nil {
		fmt.Fprintf(&result, " (after: %s)", m.formatDateTime(*afterTime))
	}
	result.WriteString(":\n\n")

	for i := len(messages) - 1; i >= 0; i-- { // reverse to show oldest first
		msg := messages[i]
		sender := getSenderDisplayName(msg)

		direction := "←"
		if msg.IsFromMe {
			direction = "→"
			sender = "You"
		}

		fmt.Fprintf(&result, "[%s] %s %s: %s\n",
			m.formatTime(msg.Timestamp),
			direction,
			sender,
			msg.Text)

		// show media metadata if present
		if msg.MediaMetadata != nil {
			meta := msg.MediaMetadata
			fmt.Fprintf(&result, "   📎 %s (%s, %s)",
				meta.FileName, meta.MimeType, formatFileSize(meta.FileSize))

			// add dimensions if available
			if dims := formatDimensions(meta.Width, meta.Height); dims != "" {
				fmt.Fprintf(&result, ", %s", dims)
			}

			// add duration if available
			if dur := formatDuration(meta.Duration); dur != "" {
				fmt.Fprintf(&result, ", %s", dur)
			}

			// show download status
			switch meta.DownloadStatus {
			case "downloaded":
				result.WriteString(" [Downloaded]")
				fmt.Fprintf(&result, "\n   Resource: whatsapp://media/%s", msg.ID)
			case "pending":
				result.WriteString(" [Not downloaded]")
			case "failed":
				result.WriteString(" [Download failed]")
			case "expired":
				result.WriteString(" [Expired]")
			}
			result.WriteString("\n")
		}
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleSearchMessages handles the search_messages tool request.
func (m *MCPServer) handleSearchMessages(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// get query (can be empty when using 'from' parameter)
	query := request.GetString("query", "")

	// get optional limit
	limit := request.GetFloat("limit", 50.0)
	if limit > 200 {
		limit = 200
	}

	// get optional sender filter
	senderJID := request.GetString("from", "")

	// validate: must have either query or from
	if query == "" && senderJID == "" {
		return mcp.NewToolResultError("must provide either 'query' (text to search) or 'from' (sender JID) or both"), nil
	}

	// detect pattern type
	useGlob := detectPatternType(query)

	// search database
	messages, err := m.store.SearchMessagesWithNamesFiltered(query, useGlob, senderJID, int(limit))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// format response
	var result strings.Builder
	fmt.Fprintf(&result, "Found %d messages matching '%s'", len(messages), query)
	if senderJID != "" {
		fmt.Fprintf(&result, " from sender %s", senderJID)
	}
	if useGlob {
		result.WriteString(" (using pattern matching)")
	}
	result.WriteString(":\n\n")

	for i, msg := range messages {
		sender := getSenderDisplayName(msg)

		if msg.IsFromMe {
			sender = "You"
		}

		fmt.Fprintf(&result, "%d. [%s] %s in chat %s:\n",
			i+1,
			m.formatDateTime(msg.Timestamp),
			sender,
			msg.ChatJID)
		fmt.Fprintf(&result, "   %s\n", msg.Text)

		// show media metadata if present
		if msg.MediaMetadata != nil {
			meta := msg.MediaMetadata
			fmt.Fprintf(&result, "   📎 %s (%s, %s)",
				meta.FileName, meta.MimeType, formatFileSize(meta.FileSize))

			// add dimensions if available
			if dims := formatDimensions(meta.Width, meta.Height); dims != "" {
				fmt.Fprintf(&result, ", %s", dims)
			}

			// add duration if available
			if dur := formatDuration(meta.Duration); dur != "" {
				fmt.Fprintf(&result, ", %s", dur)
			}

			// show download status
			switch meta.DownloadStatus {
			case "downloaded":
				result.WriteString(" [Downloaded]")
				fmt.Fprintf(&result, "\n   Resource: whatsapp://media/%s", msg.ID)
			case "pending":
				result.WriteString(" [Not downloaded]")
			case "failed":
				result.WriteString(" [Download failed]")
			case "expired":
				result.WriteString(" [Expired]")
			}
			result.WriteString("\n")
		}

		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleFindChat handles the find_chat tool request.
func (m *MCPServer) handleFindChat(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// get required search parameter
	search, err := request.RequireString("search")
	if err != nil {
		return mcp.NewToolResultError("search parameter is required"), nil
	}

	// detect pattern type
	useGlob := detectPatternType(search)

	// search chats in database
	chats, err := m.store.SearchChatsFiltered(search, useGlob, 100)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to search chats: %v", err)), nil
	}

	// format response
	var result strings.Builder
	fmt.Fprintf(&result, "Found %d matching chats", len(chats))
	if useGlob {
		result.WriteString(" (using pattern matching)")
	}
	result.WriteString(":\n\n")

	for i, chat := range chats {
		chatType := "DM"
		if chat.IsGroup {
			chatType = "Group"
		}

		displayName := getDisplayName(chat)
		fmt.Fprintf(&result, "%d. [%s] %s\n", i+1, chatType, displayName)
		fmt.Fprintf(&result, "   JID: %s\n", chat.JID)
		if chat.ContactName != "" && chat.PushName != "" && chat.ContactName != chat.PushName {
			fmt.Fprintf(&result, "   (Contact: %s, Push: %s)\n", chat.ContactName, chat.PushName)
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleSendMessage handles the send_message tool request.
func (m *MCPServer) handleSendMessage(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// get required parameters
	chatJID, err := request.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError("chat_jid parameter is required"), nil
	}

	text, err := request.RequireString("text")
	if err != nil {
		return mcp.NewToolResultError("text parameter is required"), nil
	}

	// check WhatsApp connection
	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	// get optional reply_to parameter
	replyTo := request.GetString("reply_to", "")

	// send message
	err = m.wa.SendTextMessage(ctx, chatJID, text, replyTo)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Message sent successfully to %s", chatJID)), nil
}

// handleSendImage handles the send_image tool request.
func (m *MCPServer) handleSendImage(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatJID, err := request.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError("chat_jid parameter is required"), nil
	}

	imageURL, err := request.RequireString("image_url")
	if err != nil {
		return mcp.NewToolResultError("image_url parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	caption := request.GetString("caption", "")
	replyTo := request.GetString("reply_to", "")

	err = m.wa.SendImageMessage(ctx, chatJID, imageURL, caption, replyTo)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to send image: %v", err)), nil
	}

	mediaType := "Image"
	if strings.Contains(strings.ToLower(imageURL), ".gif") {
		mediaType = "GIF"
	}

	return mcp.NewToolResultText(fmt.Sprintf("%s sent successfully to %s", mediaType, chatJID)), nil
}

// handleSendVideo handles the send_video tool request.
func (m *MCPServer) handleSendVideo(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatJID, err := request.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError("chat_jid parameter is required"), nil
	}

	video, err := request.RequireString("video")
	if err != nil {
		return mcp.NewToolResultError("video parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	caption := request.GetString("caption", "")
	replyTo := request.GetString("reply_to", "")

	err = m.wa.SendVideoMessage(ctx, chatJID, video, caption, replyTo)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to send video: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Video sent successfully to %s", chatJID)), nil
}

// handleLoadMoreMessages handles the load_more_messages tool request.
func (m *MCPServer) handleLoadMoreMessages(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// get required chat_jid
	chatJID, err := request.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError("chat_jid parameter is required"), nil
	}

	// get optional count (default 50, max 200)
	count := int(request.GetFloat("count", 50.0))
	if count > 200 {
		count = 200
	}
	if count < 1 {
		count = 1
	}

	// get optional wait_for_sync (default true)
	waitForSync := request.GetBool("wait_for_sync", true)

	// check WhatsApp connection
	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	// request history sync
	messages, err := m.wa.RequestHistorySync(ctx, chatJID, count, waitForSync)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to load messages: %v", err)), nil
	}

	// format response
	var result strings.Builder

	if waitForSync {
		fmt.Fprintf(&result, "Loaded %d additional messages from chat %s:\n\n", len(messages), chatJID)

		// format messages (oldest first, like get_chat_messages)
		for i := len(messages) - 1; i >= 0; i-- {
			msg := messages[i]
			sender := getSenderDisplayName(msg)

			direction := "←"
			if msg.IsFromMe {
				direction = "→"
				sender = "You"
			}

			fmt.Fprintf(&result, "[%s] %s %s: %s\n",
				m.formatTime(msg.Timestamp),
				direction,
				sender,
				msg.Text)

			// show media metadata if present
			if msg.MediaMetadata != nil {
				meta := msg.MediaMetadata
				fmt.Fprintf(&result, "   📎 %s (%s, %s)",
					meta.FileName, meta.MimeType, formatFileSize(meta.FileSize))

				// add dimensions if available
				if dims := formatDimensions(meta.Width, meta.Height); dims != "" {
					fmt.Fprintf(&result, ", %s", dims)
				}

				// add duration if available
				if dur := formatDuration(meta.Duration); dur != "" {
					fmt.Fprintf(&result, ", %s", dur)
				}

				// show download status
				switch meta.DownloadStatus {
				case "downloaded":
					result.WriteString(" [Downloaded]")
				case "pending":
					result.WriteString(" [Not downloaded]")
				case "failed":
					result.WriteString(" [Download failed]")
				case "expired":
					result.WriteString(" [Expired]")
				}
				result.WriteString("\n")
			}
		}
	} else {
		fmt.Fprintf(&result, "History sync request sent for chat %s (%d messages). Messages will load in the background. Use get_chat_messages to see them once loaded.", chatJID, count)
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleGetMyInfo handles the get_my_info tool request.
func (m *MCPServer) handleGetMyInfo(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// check WhatsApp connection
	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	// get user info
	myInfo, err := m.wa.GetMyInfo(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get user info: %v", err)), nil
	}

	// format response
	var result strings.Builder
	fmt.Fprintf(&result, "Your WhatsApp Profile:\n\n")
	fmt.Fprintf(&result, "JID: %s\n", myInfo.JID)

	if myInfo.PushName != "" {
		fmt.Fprintf(&result, "Display Name: %s\n", myInfo.PushName)
	}

	if myInfo.Status != "" {
		fmt.Fprintf(&result, "Status/Bio: %s\n", myInfo.Status)
	} else {
		fmt.Fprintf(&result, "Status/Bio: (not set)\n")
	}

	if myInfo.BusinessName != "" {
		fmt.Fprintf(&result, "Business Name: %s\n", myInfo.BusinessName)
	}

	if myInfo.PictureURL != "" {
		fmt.Fprintf(&result, "\nProfile Picture:\n")
		fmt.Fprintf(&result, "  Picture ID: %s\n", myInfo.PictureID)
		fmt.Fprintf(&result, "  URL: %s\n", myInfo.PictureURL)
	} else {
		fmt.Fprintf(&result, "\nProfile Picture: (not set)\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

// ── Community handlers ──────────────────────────────────────────────────────

// handleCreateCommunity creates a new WhatsApp community.
func (m *MCPServer) handleCreateCommunity(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := request.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	info, err := m.wa.CreateCommunity(ctx, name)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create community: %v", err)), nil
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Community created successfully!\n\n")
	fmt.Fprintf(&result, "Name: %s\n", info.GroupName.Name)
	fmt.Fprintf(&result, "JID: %s\n", info.JID.String())
	fmt.Fprintf(&result, "Owner: %s\n", info.OwnerJID.String())
	fmt.Fprintf(&result, "Created: %s\n", m.formatDateTime(info.GroupCreated))
	fmt.Fprintf(&result, "Participants: %d\n", len(info.Participants))

	return mcp.NewToolResultText(result.String()), nil
}

// handleCreateCommunityGroup creates a group inside a community.
func (m *MCPServer) handleCreateCommunityGroup(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	communityJID, err := request.RequireString("community_jid")
	if err != nil {
		return mcp.NewToolResultError("community_jid parameter is required"), nil
	}

	name, err := request.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	info, err := m.wa.CreateCommunityGroup(ctx, communityJID, name)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create community group: %v", err)), nil
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Community group created successfully!\n\n")
	fmt.Fprintf(&result, "Name: %s\n", info.GroupName.Name)
	fmt.Fprintf(&result, "JID: %s\n", info.JID.String())
	fmt.Fprintf(&result, "Parent community: %s\n", communityJID)

	return mcp.NewToolResultText(result.String()), nil
}

// handleListCommunityGroups lists all sub-groups of a community.
func (m *MCPServer) handleListCommunityGroups(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	communityJID, err := request.RequireString("community_jid")
	if err != nil {
		return mcp.NewToolResultError("community_jid parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	groups, err := m.wa.ListCommunityGroups(ctx, communityJID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list community groups: %v", err)), nil
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Community %s has %d sub-groups:\n\n", communityJID, len(groups))

	for i, g := range groups {
		defaultTag := ""
		if g.IsDefaultSubGroup {
			defaultTag = " [default/announcements]"
		}
		fmt.Fprintf(&result, "%d. %s%s\n", i+1, g.GroupName.Name, defaultTag)
		fmt.Fprintf(&result, "   JID: %s\n\n", g.JID.String())
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleUnlinkCommunityGroup removes a group from a community.
func (m *MCPServer) handleUnlinkCommunityGroup(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	communityJID, err := request.RequireString("community_jid")
	if err != nil {
		return mcp.NewToolResultError("community_jid parameter is required"), nil
	}

	groupJID, err := request.RequireString("group_jid")
	if err != nil {
		return mcp.NewToolResultError("group_jid parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	err = m.wa.UnlinkGroupFromCommunity(ctx, communityJID, groupJID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to unlink group: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Group %s removed from community %s", groupJID, communityJID)), nil
}

// handleLinkCommunityGroup adds an existing group to a community.
func (m *MCPServer) handleLinkCommunityGroup(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	communityJID, err := request.RequireString("community_jid")
	if err != nil {
		return mcp.NewToolResultError("community_jid parameter is required"), nil
	}

	groupJID, err := request.RequireString("group_jid")
	if err != nil {
		return mcp.NewToolResultError("group_jid parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	err = m.wa.LinkGroupToCommunity(ctx, communityJID, groupJID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to link group: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Group %s added to community %s", groupJID, communityJID)), nil
}

// handleGetCommunityInfo returns info about a community.
func (m *MCPServer) handleGetCommunityInfo(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	communityJID, err := request.RequireString("community_jid")
	if err != nil {
		return mcp.NewToolResultError("community_jid parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	info, err := m.wa.GetCommunityInfo(ctx, communityJID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get community info: %v", err)), nil
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Community: %s\n", info.GroupName.Name)
	fmt.Fprintf(&result, "JID: %s\n", info.JID.String())
	fmt.Fprintf(&result, "Owner: %s\n", info.OwnerJID.String())
	fmt.Fprintf(&result, "Created: %s\n", m.formatDateTime(info.GroupCreated))
	fmt.Fprintf(&result, "Participants: %d\n", len(info.Participants))
	if info.GroupTopic.Topic != "" {
		fmt.Fprintf(&result, "Description: %s\n", info.GroupTopic.Topic)
	}
	fmt.Fprintf(&result, "Locked: %v\n", info.IsLocked)
	fmt.Fprintf(&result, "Announcements only: %v\n", info.IsAnnounce)

	if len(info.Participants) > 0 {
		result.WriteString("\nParticipants:\n")
		for _, p := range info.Participants {
			role := "member"
			if p.IsSuperAdmin {
				role = "super-admin"
			} else if p.IsAdmin {
				role = "admin"
			}
			fmt.Fprintf(&result, "  - %s (%s)\n", p.JID.String(), role)
		}
	}

	return mcp.NewToolResultText(result.String()), nil
}

// ── Profile picture handlers ────────────────────────────────────────────────

// handleGetProfilePicture returns the profile picture URL for a single JID.
func (m *MCPServer) handleGetProfilePicture(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	jid, err := request.RequireString("jid")
	if err != nil {
		return mcp.NewToolResultError("jid parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	pic, err := m.wa.GetProfilePicture(ctx, jid)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get profile picture: %v", err)), nil
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Profile picture for %s:\n\n", jid)

	if pic.URL != "" {
		fmt.Fprintf(&result, "Picture ID: %s\n", pic.PictureID)
		fmt.Fprintf(&result, "URL: %s\n", pic.URL)
	} else if pic.Error != "" {
		fmt.Fprintf(&result, "Not available: %s\n", pic.Error)
	} else {
		fmt.Fprintf(&result, "No profile picture set.\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleGetContactProfilePictures returns profile picture URLs for multiple JIDs.
func (m *MCPServer) handleGetContactProfilePictures(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	jidsStr, err := request.RequireString("jids")
	if err != nil {
		return mcp.NewToolResultError("jids parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	// Split comma-separated JIDs and trim whitespace
	rawJIDs := strings.Split(jidsStr, ",")
	jids := make([]string, 0, len(rawJIDs))
	for _, j := range rawJIDs {
		trimmed := strings.TrimSpace(j)
		if trimmed != "" {
			jids = append(jids, trimmed)
		}
	}

	if len(jids) == 0 {
		return mcp.NewToolResultError("no valid JIDs provided"), nil
	}

	pics, err := m.wa.GetProfilePictures(ctx, jids)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get profile pictures: %v", err)), nil
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Profile pictures for %d contacts:\n\n", len(pics))

	for i, pic := range pics {
		fmt.Fprintf(&result, "%d. %s\n", i+1, pic.JID)
		if pic.URL != "" {
			fmt.Fprintf(&result, "   Picture ID: %s\n", pic.PictureID)
			fmt.Fprintf(&result, "   URL: %s\n", pic.URL)
		} else if pic.Error != "" {
			fmt.Fprintf(&result, "   Not available: %s\n", pic.Error)
		} else {
			fmt.Fprintf(&result, "   No profile picture set.\n")
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

// handleDownloadMedia force-downloads skipped or failed media files.
func (m *MCPServer) handleDownloadMedia(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	messageID := request.GetString("message_id", "")

	limit := int(request.GetFloat("limit", 10.0))
	if limit > 50 {
		limit = 50
	}

	if messageID != "" {
		// download specific message
		meta, err := m.mediaStore.GetMediaMetadata(messageID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to get metadata: %v", err)), nil
		}
		if meta == nil {
			return mcp.NewToolResultError(fmt.Sprintf("no media found for message %s", messageID)), nil
		}
		if meta.DownloadStatus == "downloaded" {
			return mcp.NewToolResultText(fmt.Sprintf("Already downloaded: %s → %s", meta.FileName, meta.FilePath)), nil
		}

		filePath, err := m.wa.DownloadMediaFromMetadata(ctx, meta)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("download failed for %s: %v", meta.FileName, err)), nil
		}
		m.mediaStore.UpdateDownloadStatus(messageID, "downloaded", &filePath, nil)
		return mcp.NewToolResultText(fmt.Sprintf("Downloaded: %s → %s", meta.FileName, filePath)), nil
	}

	// download all skipped/failed
	skipped, err := m.mediaStore.GetSkippedMedia(limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list skipped media: %v", err)), nil
	}

	if len(skipped) == 0 {
		return mcp.NewToolResultText("No skipped or failed media to download."), nil
	}

	var results []string
	var dlErrors []string

	for _, meta := range skipped {
		filePath, err := m.wa.DownloadMediaFromMetadata(ctx, &meta)
		if err != nil {
			errMsg := fmt.Sprintf("FAILED %s (%s): %v", meta.FileName, meta.MessageID[:8], err)
			dlErrors = append(dlErrors, errMsg)
			m.mediaStore.UpdateDownloadStatus(meta.MessageID, "failed", nil, err)
		} else {
			m.mediaStore.UpdateDownloadStatus(meta.MessageID, "downloaded", &filePath, nil)
			results = append(results, fmt.Sprintf("OK %s → %s", meta.FileName, filePath))
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Force download results (%d files):\n\n", len(skipped))
	for _, r := range results {
		fmt.Fprintf(&sb, "✅ %s\n", r)
	}
	for _, e := range dlErrors {
		fmt.Fprintf(&sb, "❌ %s\n", e)
	}
	fmt.Fprintf(&sb, "\nTotal: %d OK, %d failed", len(results), len(dlErrors))

	return mcp.NewToolResultText(sb.String()), nil
}

// handleSendDocument handles the send_document tool request.
func (m *MCPServer) handleSendDocument(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatJID, err := request.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError("chat_jid parameter is required"), nil
	}

	filePath, err := request.RequireString("file_path")
	if err != nil {
		return mcp.NewToolResultError("file_path parameter is required"), nil
	}

	if !m.wa.IsLoggedIn() {
		return mcp.NewToolResultError("WhatsApp is not connected"), nil
	}

	fileName := request.GetString("file_name", "")
	caption := request.GetString("caption", "")
	replyTo := request.GetString("reply_to", "")

	err = m.wa.SendDocumentMessage(ctx, chatJID, filePath, fileName, caption, replyTo)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to send document: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Document sent successfully to %s", chatJID)), nil
}
