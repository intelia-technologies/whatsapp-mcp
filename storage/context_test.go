package storage

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) (*sql.DB, *MessageStore) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}

	migrator := NewMigrator(db)
	if err := migrator.Migrate(); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	store := NewMessageStore(db)
	return db, store
}

func TestMessageContextAndInteractions(t *testing.T) {
	db, store := setupTestDB(t)
	defer db.Close()

	now := time.Now().Truncate(time.Second)

	// 1. Seed chats
	chats := []struct {
		jid         string
		pushName    string
		contactName string
		lastTime    int64
		isGroup     bool
	}{
		{"34636513587@s.whatsapp.net", "David Push", "David Contact", now.Add(-10 * time.Minute).Unix(), false},
		{"34612345678@s.whatsapp.net", "Alice", "Alice C", now.Add(-30 * time.Minute).Unix(), false},
		{"1203630283749281@g.us", "David Group", "David Group Name", now.Add(-5 * time.Minute).Unix(), true},
	}

	for _, c := range chats {
		_, err := db.Exec(`
			INSERT INTO chats (jid, push_name, contact_name, last_message_time, is_group)
			VALUES (?, ?, ?, ?, ?)
		`, c.jid, c.pushName, c.contactName, c.lastTime, c.isGroup)
		if err != nil {
			t.Fatalf("failed to insert chat: %v", err)
		}
	}

	// 2. Seed messages in David's direct chat
	// Sequence: msg1 (-15m), msg2 (-14m), msg3 (-13m, target), msg4 (-12m), msg5 (-11m)
	chatJID := "34636513587@s.whatsapp.net"
	rawMsgs := []struct {
		id       string
		text     string
		offset   time.Duration
		isFromMe bool
		sender   string
	}{
		{"msg1", "Hello there", -15 * time.Minute, false, chatJID},
		{"msg2", "How are you?", -14 * time.Minute, false, chatJID},
		{"msg3", "Let's discuss the project", -13 * time.Minute, true, "me@s.whatsapp.net"},
		{"msg4", "Sounds good to me", -12 * time.Minute, false, chatJID},
		{"msg5", "See you tomorrow", -11 * time.Minute, true, "me@s.whatsapp.net"},
	}

	for _, m := range rawMsgs {
		_, err := db.Exec(`
			INSERT INTO messages (id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type)
			VALUES (?, ?, ?, ?, ?, ?, 'text')
		`, m.id, chatJID, m.sender, m.text, now.Add(m.offset).Unix(), m.isFromMe)
		if err != nil {
			t.Fatalf("failed to insert message: %v", err)
		}
	}

	// Also seed a message from David in the group chat
	_, err := db.Exec(`
		INSERT INTO messages (id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type)
		VALUES ('grp_msg1', '1203630283749281@g.us', '34636513587@s.whatsapp.net', 'Hello group from David', ?, 0, 'text')
	`, now.Add(-5*time.Minute).Unix())
	if err != nil {
		t.Fatalf("failed to insert group message: %v", err)
	}

	t.Run("GetMessageWithNamesByID", func(t *testing.T) {
		msg, err := store.GetMessageWithNamesByID("msg3")
		if err != nil {
			t.Fatalf("GetMessageWithNamesByID failed: %v", err)
		}
		if msg == nil {
			t.Fatalf("expected msg3, got nil")
		}
		if msg.Text != "Let's discuss the project" {
			t.Errorf("expected text %q, got %q", "Let's discuss the project", msg.Text)
		}
		if !msg.IsFromMe {
			t.Errorf("expected isFromMe=true")
		}

		// Non-existent message
		nonExistent, err := store.GetMessageWithNamesByID("does_not_exist")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if nonExistent != nil {
			t.Errorf("expected nil for non-existent message, got %+v", nonExistent)
		}
	})

	t.Run("GetMessageContext", func(t *testing.T) {
		ctx, err := store.GetMessageContext("msg3", 2, 2)
		if err != nil {
			t.Fatalf("GetMessageContext failed: %v", err)
		}
		if ctx == nil {
			t.Fatalf("expected context, got nil")
		}

		if ctx.Target.ID != "msg3" {
			t.Errorf("expected target ID msg3, got %s", ctx.Target.ID)
		}

		// Before should be [msg1, msg2] in chronological order
		if len(ctx.Before) != 2 {
			t.Fatalf("expected 2 before messages, got %d", len(ctx.Before))
		}
		if ctx.Before[0].ID != "msg1" || ctx.Before[1].ID != "msg2" {
			t.Errorf("expected [msg1, msg2], got [%s, %s]", ctx.Before[0].ID, ctx.Before[1].ID)
		}

		// After should be [msg4, msg5] in chronological order
		if len(ctx.After) != 2 {
			t.Fatalf("expected 2 after messages, got %d", len(ctx.After))
		}
		if ctx.After[0].ID != "msg4" || ctx.After[1].ID != "msg5" {
			t.Errorf("expected [msg4, msg5], got [%s, %s]", ctx.After[0].ID, ctx.After[1].ID)
		}
	})

	t.Run("GetLastInteraction", func(t *testing.T) {
		// Test with full JID: most recent message involving David is grp_msg1 at -5min
		lastMsg, err := store.GetLastInteraction("34636513587@s.whatsapp.net")
		if err != nil {
			t.Fatalf("GetLastInteraction failed: %v", err)
		}
		if lastMsg == nil {
			t.Fatalf("expected last interaction, got nil")
		}
		if lastMsg.ID != "grp_msg1" {
			t.Errorf("expected grp_msg1 as most recent, got %s", lastMsg.ID)
		}

		// Test with phone number pattern without @s.whatsapp.net
		lastMsgPattern, err := store.GetLastInteraction("34636513587")
		if err != nil {
			t.Fatalf("GetLastInteraction with pattern failed: %v", err)
		}
		if lastMsgPattern == nil || lastMsgPattern.ID != "grp_msg1" {
			t.Errorf("expected grp_msg1, got %+v", lastMsgPattern)
		}
	})

	t.Run("GetContactChats", func(t *testing.T) {
		chats, err := store.GetContactChats("34636513587@s.whatsapp.net", 10)
		if err != nil {
			t.Fatalf("GetContactChats failed: %v", err)
		}
		// David is involved in 2 chats: direct chat and group chat
		if len(chats) != 2 {
			t.Fatalf("expected 2 chats involving David, got %d", len(chats))
		}

		// Group chat has last_message_time at -5m, direct chat at -10m
		if chats[0].JID != "1203630283749281@g.us" {
			t.Errorf("expected group chat first, got %s", chats[0].JID)
		}
		if chats[1].JID != "34636513587@s.whatsapp.net" {
			t.Errorf("expected direct chat second, got %s", chats[1].JID)
		}
	})

	t.Run("GetDirectChatByContact", func(t *testing.T) {
		// By phone number
		chat, err := store.GetDirectChatByContact("34636513587")
		if err != nil {
			t.Fatalf("GetDirectChatByContact by phone failed: %v", err)
		}
		if chat == nil || chat.JID != "34636513587@s.whatsapp.net" {
			t.Errorf("expected David direct chat, got %+v", chat)
		}

		// By name
		chatByName, err := store.GetDirectChatByContact("David Contact")
		if err != nil {
			t.Fatalf("GetDirectChatByContact by contact name failed: %v", err)
		}
		if chatByName == nil || chatByName.JID != "34636513587@s.whatsapp.net" {
			t.Errorf("expected David direct chat, got %+v", chatByName)
		}

		// Group name should NOT match
		chatGroupMatch, err := store.GetDirectChatByContact("David Group")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Shouldn't return the group chat even if name contains David
		if chatGroupMatch != nil && chatGroupMatch.IsGroup {
			t.Errorf("expected non-group chat or nil, got group chat: %+v", chatGroupMatch)
		}
	})
}
