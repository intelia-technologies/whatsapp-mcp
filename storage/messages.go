package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Message represents a WhatsApp message.
type Message struct {
	ID          string
	ChatJID     string // Canonical JID
	SenderJID   string // Canonical JID
	Text        string
	Timestamp   time.Time
	IsFromMe    bool
	MessageType string
	ReplyToID   string // ID of the message this is replying to or reacting to (optional)
}

// ReferralInfo holds Click-to-WhatsApp (CTWA) ad referral metadata extracted from
// ExternalAdReply ContextInfo. It is not persisted to the database.
type ReferralInfo struct {
	CtwaClid   string `json:"ctwa_clid,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	SourceURL  string `json:"source_url,omitempty"`
	Headline   string `json:"headline,omitempty"`
}

// MessageWithNames represents a message with sender and chat names from the database view.
type MessageWithNames struct {
	Message
	SenderPushName    string         // Current WhatsApp display name (from push_names table)
	SenderContactName string         // Current saved contact name (from chats table)
	ChatName          string         // Current chat name (for display)
	MediaMetadata     *MediaMetadata // Associated media metadata (null if no media)
	Referral          *ReferralInfo  // CTWA ad referral metadata (null if no ad referral)
}

// MessageStore handles message operations on the database.
type MessageStore struct {
	db *sql.DB
}

// NewMessageStore creates a new message store instance.
func NewMessageStore(db *sql.DB) *MessageStore {
	return &MessageStore{db: db}
}

// SaveMessage saves a WhatsApp message to the database.
func (s *MessageStore) SaveMessage(msg Message) error {
	query := `
	INSERT INTO messages
	(id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type, reply_to_id)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		chat_jid = excluded.chat_jid,
		sender_jid = excluded.sender_jid,
		text = excluded.text,
		timestamp = excluded.timestamp,
		is_from_me = excluded.is_from_me,
		message_type = excluded.message_type,
		reply_to_id = excluded.reply_to_id
	`

	// Use nil for empty reply_to_id
	var replyToID interface{}
	if msg.ReplyToID != "" {
		replyToID = msg.ReplyToID
	}

	_, err := s.db.Exec(
		query,
		msg.ID,
		msg.ChatJID,
		msg.SenderJID,
		msg.Text,
		msg.Timestamp.Unix(),
		msg.IsFromMe,
		msg.MessageType,
		replyToID,
	)

	if err != nil {
		return fmt.Errorf("failed to save message: %w", err)
	}

	return nil
}

// SaveBulk saves multiple messages in a single transaction.
// This is optimized for history sync operations.
func (s *MessageStore) SaveBulk(messages []Message) error {
	tx, err := s.db.Begin()

	if err != nil {
		return err
	}

	defer tx.Rollback()

	stmt, err := tx.Prepare(`
	INSERT INTO messages
	(id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type, reply_to_id)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		chat_jid = excluded.chat_jid,
		sender_jid = excluded.sender_jid,
		text = excluded.text,
		timestamp = excluded.timestamp,
		is_from_me = excluded.is_from_me,
		message_type = excluded.message_type,
		reply_to_id = excluded.reply_to_id
	`)
	if err != nil {
		return err
	}

	defer stmt.Close()

	for _, msg := range messages {
		// Use nil for empty reply_to_id
		var replyToID interface{}
		if msg.ReplyToID != "" {
			replyToID = msg.ReplyToID
		}

		_, err := stmt.Exec(
			msg.ID,
			msg.ChatJID,
			msg.SenderJID,
			msg.Text,
			msg.Timestamp.Unix(),
			msg.IsFromMe,
			msg.MessageType,
			replyToID,
		)

		if err != nil {
			return fmt.Errorf("failed to insert message %s: %w", msg.ID, err)
		}
	}

	return tx.Commit()

}

// SearchMessages searches messages by text content.
func (s *MessageStore) SearchMessages(q string, limit int) ([]Message, error) {
	query := `
	SELECT id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type
	FROM messages
	WHERE text LIKE ?
	ORDER BY timestamp DESC
	LIMIT ?
	`

	rows, err := s.db.Query(query, "%"+q+"%", limit)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	return s.scanMessages(rows)
}

// GetChatMessages retrieves messages from a specific chat.
func (s *MessageStore) GetChatMessages(chatJID string, limit int, offset int) ([]Message, error) {
	query := `
	SELECT id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type
	FROM messages
	WHERE chat_jid = ?
	ORDER BY timestamp DESC
	LIMIT ? OFFSET ?
	`

	rows, err := s.db.Query(query, chatJID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return s.scanMessages(rows)
}

// GetMessageByID retrieves a message by its ID.
// It returns nil if the message is not found.
func (s *MessageStore) GetMessageByID(messageID string) (*Message, error) {
	query := `
	SELECT id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type
	FROM messages
	WHERE id = ?
	`

	row := s.db.QueryRow(query, messageID)

	var msg Message
	var timestampUnix int64

	err := row.Scan(
		&msg.ID,
		&msg.ChatJID,
		&msg.SenderJID,
		&msg.Text,
		&timestampUnix,
		&msg.IsFromMe,
		&msg.MessageType,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	msg.Timestamp = time.Unix(timestampUnix, 0)

	return &msg, nil
}

// GetOldestMessage retrieves the oldest message from a specific chat.
// This is used for history sync requests.
func (s *MessageStore) GetOldestMessage(chatJID string) (*Message, error) {
	query := `
	SELECT id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type
	FROM messages
	WHERE chat_jid = ?
	ORDER BY timestamp ASC
	LIMIT 1
	`

	row := s.db.QueryRow(query, chatJID)

	var msg Message
	var timestampUnix int64

	err := row.Scan(
		&msg.ID,
		&msg.ChatJID,
		&msg.SenderJID,
		&msg.Text,
		&timestampUnix,
		&msg.IsFromMe,
		&msg.MessageType,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	msg.Timestamp = time.Unix(timestampUnix, 0)

	return &msg, nil
}

// GetChatMessagesOlderThan retrieves messages older than a specific timestamp.
// This is used for retrieving newly loaded messages from history sync.
func (s *MessageStore) GetChatMessagesOlderThan(chatJID string, timestamp time.Time, limit int) ([]MessageWithNames, error) {
	query := `
	SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
	       text, timestamp, is_from_me, message_type,
	       media_file_path, media_file_name, media_file_size, media_mime_type,
	       media_width, media_height, media_duration, media_download_status,
	       media_download_timestamp, media_download_error
	FROM messages_with_names
	WHERE chat_jid = ? AND timestamp < ?
	ORDER BY timestamp DESC
	LIMIT ?
	`

	rows, err := s.db.Query(query, chatJID, timestamp.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return s.scanMessagesWithNames(rows)
}

// GetChatMessagesWithNamesFiltered retrieves chat messages with advanced filtering.
func (s *MessageStore) GetChatMessagesWithNamesFiltered(
	chatJID string,
	limit int,
	beforeTimestamp *time.Time,
	afterTimestamp *time.Time,
	senderJID string,
) ([]MessageWithNames, error) {
	query := `
	SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
	       text, timestamp, is_from_me, message_type,
	       media_file_path, media_file_name, media_file_size, media_mime_type,
	       media_width, media_height, media_duration, media_download_status,
	       media_download_timestamp, media_download_error
	FROM messages_with_names
	WHERE chat_jid = ?
	`

	args := []any{chatJID}

	// add timestamp filters
	if beforeTimestamp != nil {
		query += " AND timestamp < ?"
		args = append(args, beforeTimestamp.Unix())
	}

	if afterTimestamp != nil {
		query += " AND timestamp > ?"
		args = append(args, afterTimestamp.Unix())
	}

	// add sender filter
	if senderJID != "" {
		query += " AND sender_jid = ?"
		args = append(args, senderJID)
	}

	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return s.scanMessagesWithNames(rows)
}

// SaveMessageProto stores the serialized protobuf for a sent message.
// This is used for retry receipt handling when the recipient can't decrypt.
func (s *MessageStore) SaveMessageProto(messageID string, protoBytes []byte) error {
	_, err := s.db.Exec(
		"UPDATE messages SET message_proto = ? WHERE id = ?",
		protoBytes, messageID,
	)
	return err
}

// GetMessageProto retrieves the serialized protobuf for a message.
// Returns nil if the message or proto is not found.
func (s *MessageStore) GetMessageProto(messageID string) ([]byte, error) {
	var protoBytes []byte
	err := s.db.QueryRow(
		"SELECT message_proto FROM messages WHERE id = ?",
		messageID,
	).Scan(&protoBytes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return protoBytes, nil
}

// scanMessages converts SQL rows into Message objects.
func (s *MessageStore) scanMessages(rows *sql.Rows) ([]Message, error) {
	var messages []Message

	for rows.Next() {
		var msg Message
		var timestampUnix int64

		err := rows.Scan(
			&msg.ID,
			&msg.ChatJID,
			&msg.SenderJID,
			&msg.Text,
			&timestampUnix,
			&msg.IsFromMe,
			&msg.MessageType,
		)
		if err != nil {
			return nil, err
		}

		msg.Timestamp = time.Unix(timestampUnix, 0)
		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

// SearchMessagesWithNamesFiltered searches messages with pattern matching and sender filtering.
// It uses GLOB patterns if useGlob is true, otherwise uses LIKE for fuzzy matching.
func (s *MessageStore) SearchMessagesWithNamesFiltered(
	query string,
	useGlob bool,
	senderJID string,
	limit int,
) ([]MessageWithNames, error) {
	var sqlQuery string
	var args []any

	// choose LIKE or GLOB based on pattern type
	if useGlob {
		sqlQuery = `
		SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
		       text, timestamp, is_from_me, message_type,
		       media_file_path, media_file_name, media_file_size, media_mime_type,
		       media_width, media_height, media_duration, media_download_status,
		       media_download_timestamp, media_download_error
		FROM messages_with_names
		WHERE text GLOB ?
		`
		args = append(args, query)
	} else {
		sqlQuery = `
		SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
		       text, timestamp, is_from_me, message_type,
		       media_file_path, media_file_name, media_file_size, media_mime_type,
		       media_width, media_height, media_duration, media_download_status,
		       media_download_timestamp, media_download_error
		FROM messages_with_names
		WHERE text LIKE ?
		`
		args = append(args, "%"+query+"%")
	}

	// add sender filter
	if senderJID != "" {
		sqlQuery += " AND sender_jid = ?"
		args = append(args, senderJID)
	}

	sqlQuery += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return s.scanMessagesWithNames(rows)
}

// SearchMessagesWithNames searches messages and includes sender names from view
func (s *MessageStore) SearchMessagesWithNames(q string, limit int) ([]MessageWithNames, error) {
	query := `
	SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
	       text, timestamp, is_from_me, message_type,
	       media_file_path, media_file_name, media_file_size, media_mime_type,
	       media_width, media_height, media_duration, media_download_status,
	       media_download_timestamp, media_download_error
	FROM messages_with_names
	WHERE text LIKE ?
	ORDER BY timestamp DESC
	LIMIT ?
	`

	rows, err := s.db.Query(query, "%"+q+"%", limit)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	return s.scanMessagesWithNames(rows)
}

// GetChatMessagesWithNames gets chat messages and includes sender names from view
func (s *MessageStore) GetChatMessagesWithNames(chatJID string, limit int, offset int) ([]MessageWithNames, error) {
	query := `
	SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
	       text, timestamp, is_from_me, message_type,
	       media_file_path, media_file_name, media_file_size, media_mime_type,
	       media_width, media_height, media_duration, media_download_status,
	       media_download_timestamp, media_download_error
	FROM messages_with_names
	WHERE chat_jid = ?
	ORDER BY timestamp DESC
	LIMIT ? OFFSET ?
	`

	rows, err := s.db.Query(query, chatJID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return s.scanMessagesWithNames(rows)
}

// scanMessagesWithNames converts SQL rows into MessageWithNames objects.
func (s *MessageStore) scanMessagesWithNames(rows *sql.Rows) ([]MessageWithNames, error) {
	var messages []MessageWithNames

	for rows.Next() {
		var msg MessageWithNames
		var timestampUnix int64

		// nullable name/text fields (LEFT JOINs in the view can yield NULL)
		var senderPushName, senderContactName, chatName, text sql.NullString

		// media metadata fields (nullable)
		var mediaFilePath, mediaFileName, mediaMimeType sql.NullString
		var mediaFileSize sql.NullInt64
		var mediaWidth, mediaHeight, mediaDuration sql.NullInt64
		var mediaDownloadStatus, mediaDownloadError sql.NullString
		var mediaDownloadTimestamp sql.NullInt64

		err := rows.Scan(
			&msg.ID,
			&msg.ChatJID,
			&msg.SenderJID,
			&senderPushName,
			&senderContactName,
			&chatName,
			&text,
			&timestampUnix,
			&msg.IsFromMe,
			&msg.MessageType,
			// media metadata fields
			&mediaFilePath,
			&mediaFileName,
			&mediaFileSize,
			&mediaMimeType,
			&mediaWidth,
			&mediaHeight,
			&mediaDuration,
			&mediaDownloadStatus,
			&mediaDownloadTimestamp,
			&mediaDownloadError,
		)
		if err != nil {
			return nil, err
		}

		msg.Timestamp = time.Unix(timestampUnix, 0)
		msg.SenderPushName = senderPushName.String
		msg.SenderContactName = senderContactName.String
		msg.ChatName = chatName.String
		msg.Text = text.String

		// populate media metadata if present
		if mediaFileName.Valid && mediaMimeType.Valid {
			meta := &MediaMetadata{
				MessageID:      msg.ID,
				FileName:       mediaFileName.String,
				FileSize:       mediaFileSize.Int64,
				MimeType:       mediaMimeType.String,
				DownloadStatus: "pending",
			}

			if mediaFilePath.Valid {
				meta.FilePath = mediaFilePath.String
			}
			if mediaWidth.Valid {
				w := int(mediaWidth.Int64)
				meta.Width = &w
			}
			if mediaHeight.Valid {
				h := int(mediaHeight.Int64)
				meta.Height = &h
			}
			if mediaDuration.Valid {
				d := int(mediaDuration.Int64)
				meta.Duration = &d
			}
			if mediaDownloadStatus.Valid {
				meta.DownloadStatus = mediaDownloadStatus.String
			}
			if mediaDownloadTimestamp.Valid {
				ts := time.Unix(mediaDownloadTimestamp.Int64, 0)
				meta.DownloadTimestamp = &ts
			}
			if mediaDownloadError.Valid {
				meta.DownloadError = mediaDownloadError.String
			}

			msg.MediaMetadata = meta
		}

		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

// MessageContext holds a target message and its surrounding conversational context.
type MessageContext struct {
	Target MessageWithNames
	Before []MessageWithNames
	After  []MessageWithNames
}

// GetMessageWithNamesByID retrieves a single message with names by its ID.
func (s *MessageStore) GetMessageWithNamesByID(messageID string) (*MessageWithNames, error) {
	query := `
	SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
	       text, timestamp, is_from_me, message_type,
	       media_file_path, media_file_name, media_file_size, media_mime_type,
	       media_width, media_height, media_duration, media_download_status,
	       media_download_timestamp, media_download_error
	FROM messages_with_names
	WHERE id = ?
	LIMIT 1
	`
	rows, err := s.db.Query(query, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs, err := s.scanMessagesWithNames(rows)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return &msgs[0], nil
}

// messageCreatedAt returns the row's insertion timestamp, used to break ties
// between messages that share the same one-second WhatsApp timestamp. Rows
// written before the column existed report an empty string, which still orders
// deterministically.
func (s *MessageStore) messageCreatedAt(messageID string) (string, error) {
	var createdAt string
	err := s.db.QueryRow(
		`SELECT COALESCE(created_at, '') FROM messages WHERE id = ?`,
		messageID,
	).Scan(&createdAt)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return createdAt, nil
}

// GetMessageContext retrieves a target message along with N messages before and N messages after in the same chat.
func (s *MessageStore) GetMessageContext(messageID string, before int, after int) (*MessageContext, error) {
	target, err := s.GetMessageWithNamesByID(messageID)
	if err != nil {
		return nil, fmt.Errorf("failed to get target message: %w", err)
	}
	if target == nil {
		return nil, nil
	}

	if before < 0 {
		before = 0
	}
	if before > 50 {
		before = 50
	}
	if after < 0 {
		after = 0
	}
	if after > 50 {
		after = 50
	}

	// timestamp alone cannot split the chat around the target: WhatsApp stores
	// it with one-second precision and bursts of messages routinely share a
	// second, so a strict < / > comparison silently drops every message sent in
	// the same second as the target -- 2% of this database. Falling back to
	// created_at (insertion order) and then the unique id gives a total order,
	// so every message lands on exactly one side of the split.
	createdAt, err := s.messageCreatedAt(messageID)
	if err != nil {
		return nil, fmt.Errorf("failed to get target ordering key: %w", err)
	}
	targetUnix := target.Timestamp.Unix()

	msgCtx := &MessageContext{
		Target: *target,
	}

	// Fetch messages before (ordered DESC, then reversed to chronological)
	if before > 0 {
		queryBefore := `
		SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
		       text, timestamp, is_from_me, message_type,
		       media_file_path, media_file_name, media_file_size, media_mime_type,
		       media_width, media_height, media_duration, media_download_status,
		       media_download_timestamp, media_download_error
		FROM messages_with_names
		WHERE chat_jid = ? AND (timestamp, COALESCE(created_at, ''), id) < (?, ?, ?)
		ORDER BY timestamp DESC, COALESCE(created_at, '') DESC, id DESC
		LIMIT ?
		`
		rows, err := s.db.Query(queryBefore, target.ChatJID, targetUnix, createdAt, messageID, before)
		if err != nil {
			return nil, fmt.Errorf("failed to get before messages: %w", err)
		}
		defer rows.Close()

		beforeMsgs, err := s.scanMessagesWithNames(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan before messages: %w", err)
		}
		// reverse so they are ordered chronologically (oldest to newest)
		for i, j := 0, len(beforeMsgs)-1; i < j; i, j = i+1, j-1 {
			beforeMsgs[i], beforeMsgs[j] = beforeMsgs[j], beforeMsgs[i]
		}
		msgCtx.Before = beforeMsgs
	}

	// Fetch messages after (ordered ASC)
	if after > 0 {
		queryAfter := `
		SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
		       text, timestamp, is_from_me, message_type,
		       media_file_path, media_file_name, media_file_size, media_mime_type,
		       media_width, media_height, media_duration, media_download_status,
		       media_download_timestamp, media_download_error
		FROM messages_with_names
		WHERE chat_jid = ? AND (timestamp, COALESCE(created_at, ''), id) > (?, ?, ?)
		ORDER BY timestamp ASC, COALESCE(created_at, '') ASC, id ASC
		LIMIT ?
		`
		rows, err := s.db.Query(queryAfter, target.ChatJID, targetUnix, createdAt, messageID, after)
		if err != nil {
			return nil, fmt.Errorf("failed to get after messages: %w", err)
		}
		defer rows.Close()

		afterMsgs, err := s.scanMessagesWithNames(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan after messages: %w", err)
		}
		msgCtx.After = afterMsgs
	}

	return msgCtx, nil
}

// GetLastInteraction retrieves the most recent message involving a specific contact (as sender or in direct chat).
func (s *MessageStore) GetLastInteraction(jid string) (*MessageWithNames, error) {
	searchJID := strings.TrimSpace(jid)
	isPattern := !strings.Contains(searchJID, "@")

	var query string
	var args []any

	if isPattern {
		clean := strings.TrimPrefix(searchJID, "+")
		pattern := "%" + clean + "%"
		query = `
		SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
		       text, timestamp, is_from_me, message_type,
		       media_file_path, media_file_name, media_file_size, media_mime_type,
		       media_width, media_height, media_duration, media_download_status,
		       media_download_timestamp, media_download_error
		FROM messages_with_names
		WHERE sender_jid LIKE ? OR (chat_jid LIKE ? AND chat_jid NOT LIKE '%@g.us')
		ORDER BY timestamp DESC
		LIMIT 1
		`
		args = []any{pattern, pattern}
	} else {
		query = `
		SELECT id, chat_jid, sender_jid, sender_push_name, sender_contact_name, chat_name,
		       text, timestamp, is_from_me, message_type,
		       media_file_path, media_file_name, media_file_size, media_mime_type,
		       media_width, media_height, media_duration, media_download_status,
		       media_download_timestamp, media_download_error
		FROM messages_with_names
		WHERE sender_jid = ? OR chat_jid = ?
		ORDER BY timestamp DESC
		LIMIT 1
		`
		args = []any{searchJID, searchJID}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs, err := s.scanMessagesWithNames(rows)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return &msgs[0], nil
}
