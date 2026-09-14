package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Chat represents a WhatsApp conversation.
type Chat struct {
	JID             string // canonical JID (required)
	PushName        string // sender's WhatsApp display name (from PushName in messages)
	ContactName     string // saved contact name (from WhatsApp contact store)
	LastMessageTime time.Time
	UnreadCount     int
	IsGroup         bool
}

// GetChatByJID retrieves a chat by its canonical JID.
// It returns nil if the chat is not found.
func (s *MessageStore) GetChatByJID(jid string) (*Chat, error) {
	query := `
	SELECT jid, COALESCE(push_name, ''), COALESCE(contact_name, ''), last_message_time, unread_count, is_group
	FROM chats
	WHERE jid = ?
	`

	row := s.db.QueryRow(query, jid)

	var chat Chat
	var lastMsgUnix int64

	err := row.Scan(
		&chat.JID,
		&chat.PushName,
		&chat.ContactName,
		&lastMsgUnix,
		&chat.UnreadCount,
		&chat.IsGroup,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	chat.LastMessageTime = time.Unix(lastMsgUnix, 0)
	return &chat, nil
}

// SaveChat saves or updates chat information in the database.
func (s *MessageStore) SaveChat(chat Chat) error {
	if chat.JID == "" {
		return fmt.Errorf("chat JID cannot be empty")
	}

	query := `
	INSERT INTO chats (jid, push_name, contact_name, last_message_time, unread_count, is_group)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(jid) DO UPDATE SET
	    push_name = COALESCE(NULLIF(excluded.push_name, ''), chats.push_name),
	    contact_name = COALESCE(NULLIF(excluded.contact_name, ''), chats.contact_name),
	    last_message_time = excluded.last_message_time,
	    unread_count = excluded.unread_count,
	    is_group = excluded.is_group
	`

	_, err := s.db.Exec(
		query,
		chat.JID,
		chat.PushName,
		chat.ContactName,
		chat.LastMessageTime.Unix(),
		chat.UnreadCount,
		chat.IsGroup,
	)

	return err
}

// ListChats returns all chats ordered by last message timestamp.
func (s *MessageStore) ListChats(limit int) ([]Chat, error) {
	query := `
	SELECT jid, COALESCE(push_name, ''), COALESCE(contact_name, ''), last_message_time, unread_count, is_group
	FROM chats
	ORDER BY last_message_time DESC
	LIMIT ?
	`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var lastMsgUnix int64

		err := rows.Scan(
			&chat.JID,
			&chat.PushName,
			&chat.ContactName,
			&lastMsgUnix,
			&chat.UnreadCount,
			&chat.IsGroup,
		)
		if err != nil {
			return nil, err
		}

		chat.LastMessageTime = time.Unix(lastMsgUnix, 0)
		chats = append(chats, chat)
	}

	return chats, rows.Err()
}

// SearchChatsFiltered searches chats with pattern matching.
// It uses GLOB patterns if useGlob is true, otherwise uses LIKE for fuzzy matching.
func (s *MessageStore) SearchChatsFiltered(search string, useGlob bool, limit int) ([]Chat, error) {
	var query string
	var searchPattern string

	// choose LIKE or GLOB based on pattern type
	if useGlob {
		query = `
		SELECT jid, COALESCE(push_name, ''), COALESCE(contact_name, ''), last_message_time, unread_count, is_group
		FROM chats
		WHERE push_name GLOB ? OR contact_name GLOB ? OR jid GLOB ?
		ORDER BY last_message_time DESC
		LIMIT ?
		`
		searchPattern = search
	} else {
		query = `
		SELECT jid, COALESCE(push_name, ''), COALESCE(contact_name, ''), last_message_time, unread_count, is_group
		FROM chats
		WHERE push_name LIKE ? OR contact_name LIKE ? OR jid LIKE ?
		ORDER BY last_message_time DESC
		LIMIT ?
		`
		searchPattern = "%" + search + "%"
	}

	rows, err := s.db.Query(query, searchPattern, searchPattern, searchPattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var lastMsgUnix int64

		err := rows.Scan(
			&chat.JID,
			&chat.PushName,
			&chat.ContactName,
			&lastMsgUnix,
			&chat.UnreadCount,
			&chat.IsGroup,
		)
		if err != nil {
			return nil, err
		}

		chat.LastMessageTime = time.Unix(lastMsgUnix, 0)
		chats = append(chats, chat)
	}

	return chats, rows.Err()
}

// SearchChats searches chats by name or JID with fuzzy matching.
func (s *MessageStore) SearchChats(search string, limit int) ([]Chat, error) {
	query := `
	SELECT jid, COALESCE(push_name, ''), COALESCE(contact_name, ''), last_message_time, unread_count, is_group
	FROM chats
	WHERE push_name LIKE ? OR contact_name LIKE ? OR jid LIKE ?
	ORDER BY last_message_time DESC
	LIMIT ?
	`

	searchPattern := "%" + search + "%"
	rows, err := s.db.Query(query, searchPattern, searchPattern, searchPattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var lastMsgUnix int64

		err := rows.Scan(
			&chat.JID,
			&chat.PushName,
			&chat.ContactName,
			&lastMsgUnix,
			&chat.UnreadCount,
			&chat.IsGroup,
		)
		if err != nil {
			return nil, err
		}

		chat.LastMessageTime = time.Unix(lastMsgUnix, 0)
		chats = append(chats, chat)
	}

	return chats, rows.Err()
}

// GetContactChats returns all distinct conversations (DMs and groups) involving a contact.
func (s *MessageStore) GetContactChats(jid string, limit int) ([]Chat, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	searchJID := strings.TrimSpace(jid)
	isPattern := !strings.Contains(searchJID, "@")

	var query string
	var args []any

	if isPattern {
		clean := strings.TrimPrefix(searchJID, "+")
		pattern := "%" + clean + "%"
		query = `
		SELECT DISTINCT c.jid, COALESCE(c.push_name, ''), COALESCE(c.contact_name, ''), c.last_message_time, c.unread_count, c.is_group
		FROM chats c
		JOIN messages m ON c.jid = m.chat_jid
		WHERE m.sender_jid LIKE ? OR c.jid LIKE ?
		ORDER BY c.last_message_time DESC
		LIMIT ?
		`
		args = []any{pattern, pattern, limit}
	} else {
		query = `
		SELECT DISTINCT c.jid, COALESCE(c.push_name, ''), COALESCE(c.contact_name, ''), c.last_message_time, c.unread_count, c.is_group
		FROM chats c
		JOIN messages m ON c.jid = m.chat_jid
		WHERE m.sender_jid = ? OR c.jid = ?
		ORDER BY c.last_message_time DESC
		LIMIT ?
		`
		args = []any{searchJID, searchJID, limit}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var lastMsgUnix int64
		err := rows.Scan(
			&chat.JID,
			&chat.PushName,
			&chat.ContactName,
			&lastMsgUnix,
			&chat.UnreadCount,
			&chat.IsGroup,
		)
		if err != nil {
			return nil, err
		}
		chat.LastMessageTime = time.Unix(lastMsgUnix, 0)
		chats = append(chats, chat)
	}
	return chats, rows.Err()
}

// FindDirectChatsByContact searches 1-on-1 chats (groups excluded) by phone
// number or contact name, best match first.
//
// It returns every candidate instead of a single best guess on purpose. The
// substring search behind it is ambiguous by nature -- "Juan" matches six
// chats in a real database -- and callers feed the result into send_message.
// Silently picking the most recent match would eventually send a private
// message to the wrong person, with no way to notice before it happened.
func (s *MessageStore) FindDirectChatsByContact(queryStr string, limit int) ([]Chat, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	clean := strings.TrimPrefix(strings.TrimSpace(queryStr), "+")
	pattern := "%" + clean + "%"
	lowered := strings.ToLower(clean)
	phonePrefix := clean + "@%"

	// Rank exact identity matches above substring hits so that passing a full
	// JID or a complete phone number still resolves to one obvious answer.
	query := `
	SELECT jid, COALESCE(push_name, ''), COALESCE(contact_name, ''), last_message_time, unread_count, is_group
	FROM chats
	WHERE is_group = 0 AND (jid LIKE ? OR push_name LIKE ? OR contact_name LIKE ?)
	ORDER BY
		CASE
			WHEN jid = ? OR jid LIKE ? THEN 0
			WHEN lower(COALESCE(contact_name, '')) = ? OR lower(COALESCE(push_name, '')) = ? THEN 1
			ELSE 2
		END,
		last_message_time DESC
	LIMIT ?
	`
	rows, err := s.db.Query(query,
		pattern, pattern, pattern,
		clean, phonePrefix,
		lowered, lowered,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var lastMsgUnix int64
		if err := rows.Scan(
			&chat.JID,
			&chat.PushName,
			&chat.ContactName,
			&lastMsgUnix,
			&chat.UnreadCount,
			&chat.IsGroup,
		); err != nil {
			return nil, err
		}
		chat.LastMessageTime = time.Unix(lastMsgUnix, 0)
		chats = append(chats, chat)
	}
	return chats, rows.Err()
}
