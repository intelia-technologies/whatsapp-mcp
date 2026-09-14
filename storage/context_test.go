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

	t.Run("FindDirectChatsByContact", func(t *testing.T) {
		// By phone number
		byPhone, err := store.FindDirectChatsByContact("34636513587", 10)
		if err != nil {
			t.Fatalf("FindDirectChatsByContact by phone failed: %v", err)
		}
		if len(byPhone) != 1 || byPhone[0].JID != "34636513587@s.whatsapp.net" {
			t.Errorf("expected only David direct chat, got %+v", byPhone)
		}

		// By name
		byName, err := store.FindDirectChatsByContact("David Contact", 10)
		if err != nil {
			t.Fatalf("FindDirectChatsByContact by contact name failed: %v", err)
		}
		if len(byName) != 1 || byName[0].JID != "34636513587@s.whatsapp.net" {
			t.Errorf("expected only David direct chat, got %+v", byName)
		}

		// Group name should NOT match
		groupMatch, err := store.FindDirectChatsByContact("David Group", 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, chat := range groupMatch {
			if chat.IsGroup {
				t.Errorf("expected no group chat, got %+v", chat)
			}
		}
	})
}

// A name that matches several people must surface all of them. Returning the
// most recent one alone is how a private message reaches the wrong person.
func TestFindDirectChatsByContactReportsEveryNamesake(t *testing.T) {
	db, store := setupTestDB(t)
	defer db.Close()

	now := time.Now().Truncate(time.Second)
	namesakes := []struct {
		jid         string
		contactName string
		offset      time.Duration
	}{
		{"34600000001@s.whatsapp.net", "Juan Perez", -30 * time.Minute},
		{"34600000002@s.whatsapp.net", "Juan Gomez", -10 * time.Minute},
		{"34600000003@s.whatsapp.net", "Juanita Lopez", -50 * time.Minute},
		{"34600000004@s.whatsapp.net", "Marta Ruiz", -5 * time.Minute},
	}
	for _, c := range namesakes {
		if _, err := db.Exec(`
			INSERT INTO chats (jid, push_name, contact_name, last_message_time, is_group)
			VALUES (?, '', ?, ?, 0)
		`, c.jid, c.contactName, now.Add(c.offset).Unix()); err != nil {
			t.Fatalf("failed to insert chat: %v", err)
		}
	}

	matches, err := store.FindDirectChatsByContact("Juan", 10)
	if err != nil {
		t.Fatalf("FindDirectChatsByContact failed: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("expected all 3 chats matching 'Juan', got %d: %+v", len(matches), matches)
	}

	// An exact name must still resolve to one obvious answer, ranked first.
	exact, err := store.FindDirectChatsByContact("Juan Perez", 10)
	if err != nil {
		t.Fatalf("FindDirectChatsByContact exact failed: %v", err)
	}
	if len(exact) != 1 || exact[0].JID != "34600000001@s.whatsapp.net" {
		t.Errorf("expected only Juan Perez, got %+v", exact)
	}

	// A full phone number outranks a more recent substring match.
	byPhone, err := store.FindDirectChatsByContact("+34600000001", 10)
	if err != nil {
		t.Fatalf("FindDirectChatsByContact by phone failed: %v", err)
	}
	if len(byPhone) == 0 || byPhone[0].JID != "34600000001@s.whatsapp.net" {
		t.Errorf("expected the exact phone match ranked first, got %+v", byPhone)
	}
}

// WhatsApp timestamps have one-second precision and bursts of messages share a
// second routinely. A strict < / > split on timestamp alone drops those
// neighbours from the context without reporting anything.
func TestGetMessageContextKeepsSameSecondMessages(t *testing.T) {
	db, store := setupTestDB(t)
	defer db.Close()

	chatJID := "34611111111@s.whatsapp.net"
	if _, err := db.Exec(`
		INSERT INTO chats (jid, push_name, contact_name, last_message_time, is_group)
		VALUES (?, 'Burst', 'Burst', ?, 0)
	`, chatJID, time.Now().Unix()); err != nil {
		t.Fatalf("failed to insert chat: %v", err)
	}

	// Five messages fired inside the same second, the middle one is the target.
	burst := time.Now().Truncate(time.Second).Add(-time.Hour)
	ids := []string{"b1", "b2", "b3", "b4", "b5"}
	for i, id := range ids {
		if _, err := db.Exec(`
			INSERT INTO messages (id, chat_jid, sender_jid, text, timestamp, is_from_me, message_type, created_at)
			VALUES (?, ?, ?, ?, ?, 0, 'text', ?)
		`, id, chatJID, chatJID, "burst "+id, burst.Unix(),
			burst.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05")); err != nil {
			t.Fatalf("failed to insert message: %v", err)
		}
	}

	msgCtx, err := store.GetMessageContext("b3", 5, 5)
	if err != nil {
		t.Fatalf("GetMessageContext failed: %v", err)
	}
	if msgCtx == nil {
		t.Fatal("expected context for b3, got nil")
	}

	if len(msgCtx.Before) != 2 {
		t.Errorf("expected b1 and b2 before the target, got %d: %+v", len(msgCtx.Before), msgCtx.Before)
	}
	if len(msgCtx.After) != 2 {
		t.Errorf("expected b4 and b5 after the target, got %d: %+v", len(msgCtx.After), msgCtx.After)
	}

	// Every message must appear exactly once across before/target/after.
	seen := map[string]int{msgCtx.Target.ID: 1}
	for _, m := range append(append([]MessageWithNames{}, msgCtx.Before...), msgCtx.After...) {
		seen[m.ID]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("message %s appears %d times in the context, want exactly 1", id, seen[id])
		}
	}
}
