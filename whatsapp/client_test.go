package whatsapp

import (
	"testing"

	"whatsapp-mcp/storage"
)

func TestResolveReactionTarget(t *testing.T) {
	incomingGroup := &storage.Message{
		ChatJID:   "120363123456789@g.us",
		SenderJID: "5511999999999@s.whatsapp.net",
		IsFromMe:  false,
	}
	ownGroup := &storage.Message{
		ChatJID:   "120363123456789@g.us",
		SenderJID: "5511888888888:27@s.whatsapp.net",
		IsFromMe:  true,
	}

	// An incoming group message must keep its author: BuildMessageKey needs it
	// to fill Participant, without which the reaction lands on nothing.
	chat, sender, err := resolveReactionTarget(incomingGroup, "MSG1", "", "")
	if err != nil {
		t.Fatalf("incoming group message: unexpected error %v", err)
	}
	if chat.String() != incomingGroup.ChatJID {
		t.Errorf("chat = %s, want %s", chat, incomingGroup.ChatJID)
	}
	if sender.String() != incomingGroup.SenderJID {
		t.Errorf("sender = %s, want %s", sender, incomingGroup.SenderJID)
	}

	// Our own message resolves to an empty sender, which is how the key is
	// marked FromMe. Passing our device JID through would set FromMe=false.
	if _, sender, err = resolveReactionTarget(ownGroup, "MSG2", "", ""); err != nil {
		t.Fatalf("own message: unexpected error %v", err)
	} else if !sender.IsEmpty() {
		t.Errorf("sender = %s, want empty", sender)
	}

	// Explicit arguments win, so a message outside local history still works.
	chat, sender, err = resolveReactionTarget(nil, "MSG3", "120363987654321@g.us", "5511777777777@s.whatsapp.net")
	if err != nil {
		t.Fatalf("explicit target: unexpected error %v", err)
	}
	if chat.String() != "120363987654321@g.us" || sender.String() != "5511777777777@s.whatsapp.net" {
		t.Errorf("explicit target resolved to chat %s sender %s", chat, sender)
	}

	if _, _, err = resolveReactionTarget(nil, "MSG4", "", ""); err == nil {
		t.Error("unknown message without chat_jid was accepted")
	}
	if _, _, err = resolveReactionTarget(nil, "", "", ""); err == nil {
		t.Error("empty message id was accepted")
	}
	if _, _, err = resolveReactionTarget(nil, "MSG5", "not a jid", ""); err == nil {
		t.Error("malformed chat JID was accepted")
	}
}
