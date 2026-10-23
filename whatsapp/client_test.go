package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// newOfflineClient builds a Client around a whatsmeow client that is never
// connected, identified as the given bot JID
func newOfflineClient(t *testing.T, botJID types.JID) *Client {
	t.Helper()
	return &Client{cli: whatsmeow.NewClient(&store.Device{ID: &botJID}, nil)}
}

func TestRevokeMessage(t *testing.T) {
	bot := types.NewJID("34936674253", types.DefaultUserServer)
	member := types.NewJID("34600111222", types.DefaultUserServer)
	group := types.NewJID("120363420531996267", types.GroupServer)

	tests := []struct {
		name            string
		msg             IncomingMessage
		wantFromMe      bool
		wantParticipant string
	}{
		{
			name:            "group message from another member carries its sender",
			msg:             IncomingMessage{ID: "MSG1", Chat: group, Sender: member, IsGroup: true},
			wantFromMe:      false,
			wantParticipant: member.String(),
		},
		{
			name: "group message from a member device drops the device part",
			msg: IncomingMessage{
				ID:      "MSG2",
				Chat:    group,
				Sender:  types.JID{User: member.User, Device: 7, Server: types.DefaultUserServer},
				IsGroup: true,
			},
			wantFromMe:      false,
			wantParticipant: member.String(),
		},
		{
			name:       "group message from the bot itself",
			msg:        IncomingMessage{ID: "MSG3", Chat: group, Sender: bot, IsGroup: true},
			wantFromMe: true,
		},
		{
			name:       "message without sender",
			msg:        IncomingMessage{ID: "MSG4", Chat: group, IsGroup: true},
			wantFromMe: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newOfflineClient(t, bot)

			protocol := client.revokeMessage(tt.msg).GetProtocolMessage()
			if protocol.GetType() != waE2E.ProtocolMessage_REVOKE {
				t.Fatalf("type = %v, want REVOKE", protocol.GetType())
			}

			key := protocol.GetKey()
			if key.GetID() != tt.msg.ID {
				t.Errorf("key ID = %q, want %q", key.GetID(), tt.msg.ID)
			}
			if key.GetRemoteJID() != tt.msg.Chat.String() {
				t.Errorf("key remote JID = %q, want %q", key.GetRemoteJID(), tt.msg.Chat.String())
			}
			if key.GetFromMe() != tt.wantFromMe {
				t.Errorf("key FromMe = %v, want %v", key.GetFromMe(), tt.wantFromMe)
			}
			if key.GetParticipant() != tt.wantParticipant {
				t.Errorf("key participant = %q, want %q", key.GetParticipant(), tt.wantParticipant)
			}
		})
	}
}
