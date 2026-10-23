package whatsapp

import (
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// IncomingMessage is the domain representation of a received WhatsApp message.
// It is what command handlers and services work with; the raw whatsmeow
// event only appears in RawMessage for quote-replies.
type IncomingMessage struct {
	ID               types.MessageID // Message ID (for replies/deletion)
	Chat             types.JID       // Conversation the message was sent in
	Sender           types.JID       // Actual author (differs from Chat in groups)
	SenderAlt        types.JID       // Alternate address of the author (phone number for a LID sender and vice versa), empty if unknown
	PushName         string          // Display name of the author ("notifyName" in the Node version)
	Body             string          // Text content (conversation or extended text)
	IsGroup          bool            // Whether the chat is a group
	MentionedJIDs    []string        // JIDs mentioned in the message (string form)
	RawMessage       *waE2E.Message  // Original message, used for quote-replies
	HasDocument      bool            // Whether the message carries a document
	DocumentMIMEType string          // Document MIME type, if HasDocument is true
	DocumentFileName string          // Document filename, if HasDocument is true
	Synthetic        bool            // True when the message is not a real incoming event (e.g. scheduled command)
}

// NewSyntheticMessage creates an IncomingMessage for a scheduled or otherwise
// internally generated command. It has no original message to quote.
func NewSyntheticMessage(chat, sender types.JID, body string) IncomingMessage {
	return IncomingMessage{
		Chat:      chat,
		Sender:    sender,
		Body:      body,
		IsGroup:   chat.Server == types.GroupServer,
		Synthetic: true,
	}
}

// NewIncomingMessage converts a whatsmeow message event into the domain type
func NewIncomingMessage(evt *events.Message) IncomingMessage {
	msg := IncomingMessage{
		ID:         evt.Info.ID,
		Chat:       evt.Info.Chat,
		Sender:     evt.Info.Sender,
		SenderAlt:  evt.Info.SenderAlt,
		PushName:   evt.Info.PushName,
		IsGroup:    evt.Info.IsGroup,
		RawMessage: evt.Message,
	}

	msg.Body = extractText(evt.Message)
	msg.MentionedJIDs = extractMentions(evt.Message)
	msg.HasDocument, msg.DocumentMIMEType, msg.DocumentFileName = extractDocument(evt.Message)

	return msg
}

// extractText returns the text body of a message, handling extended text messages
func extractText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if text := m.GetConversation(); text != "" {
		return text
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

// extractMentions returns the mentioned JIDs (string form) of a message
func extractMentions(m *waE2E.Message) []string {
	if m == nil {
		return nil
	}
	ext := m.GetExtendedTextMessage()
	if ext == nil {
		return nil
	}
	ctxInfo := ext.GetContextInfo()
	if ctxInfo == nil {
		return nil
	}
	return ctxInfo.GetMentionedJID()
}

// extractDocument returns document metadata from a message, if present.
func extractDocument(m *waE2E.Message) (hasDoc bool, mimeType, filename string) {
	if m == nil {
		return false, "", ""
	}
	doc := m.GetDocumentMessage()
	if doc == nil {
		return false, "", ""
	}
	return true, doc.GetMimetype(), doc.GetFileName()
}
