package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite" // SQLite driver for the session store (driver name: "sqlite")
)

// Client is the whatsmeow adapter. It implements the Sender interface.
type Client struct {
	cli *whatsmeow.Client
	log *slog.Logger
}

// NewClient creates a WhatsApp client with its session stored in a SQLite database
func NewClient(sessionDBPath string, log *slog.Logger) (*Client, error) {
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+sessionDBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("failed to create session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get device store: %w", err)
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	cli := whatsmeow.NewClient(deviceStore, clientLog)

	return &Client{
		cli: cli,
		log: log,
	}, nil
}

// AddEventHandler registers the event handler (the command router)
func (c *Client) AddEventHandler(handler func(interface{})) uint32 {
	return c.cli.AddEventHandler(handler)
}

// Connect establishes the connection to WhatsApp, handling the login flow
// (QR code in terminal, or pairing code if pairPhone is set and no session exists)
func (c *Client) Connect(ctx context.Context, pairPhone string) error {
	if c.cli.Store.ID == nil {
		// No stored session: login flow required
		if pairPhone != "" {
			return c.connectWithPairCode(ctx, pairPhone)
		}
		return c.connectWithQR(ctx)
	}

	// Existing session
	c.log.Info("Existing session found, connecting")
	if err := c.cli.Connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	return nil
}

func (c *Client) connectWithQR(ctx context.Context) error {
	qrChan, err := c.cli.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("failed to get QR channel: %w", err)
	}

	if err := c.cli.Connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	go func() {
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				c.log.Info("QR code received, scan it with WhatsApp")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			case "success":
				c.log.Info("QR login successful")
			case "timeout":
				c.log.Warn("QR code timed out")
			default:
				c.log.Info("QR channel event", "event", evt.Event)
			}
		}
	}()

	return nil
}

func (c *Client) connectWithPairCode(ctx context.Context, phone string) error {
	if err := c.cli.Connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	code, err := c.cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return fmt.Errorf("failed to request pairing code: %w", err)
	}
	c.log.Info("Pairing code requested, enter it in WhatsApp", "phone", phone, "code", code)
	return nil
}

// Disconnect closes the WhatsApp connection
func (c *Client) Disconnect() {
	c.cli.Disconnect()
}

// IsConnected reports whether the client is connected and logged in
func (c *Client) IsConnected() bool {
	return c.cli.IsConnected() && c.cli.IsLoggedIn()
}

// OwnJID returns the bot's own JID (empty when not logged in)
func (c *Client) OwnJID() types.JID {
	if c.cli.Store.ID == nil {
		return types.EmptyJID
	}
	return *c.cli.Store.ID
}

// ReplyText sends a text message quoting the original message.
// For synthetic messages it falls back to a plain SendText (no quote context).
func (c *Client) ReplyText(ctx context.Context, msg IncomingMessage, text string) error {
	if msg.Synthetic {
		return c.SendText(ctx, msg.Chat, text)
	}
	out := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: c.quoteContext(msg),
		},
	}
	return c.send(ctx, msg.Chat, out)
}

// SendText sends a plain text message to a JID
func (c *Client) SendText(ctx context.Context, to types.JID, text string) error {
	out := &waE2E.Message{
		Conversation: proto.String(text),
	}
	return c.send(ctx, normalizeJID(to), out)
}

// ReplyMedia sends media quoting the original message.
// For synthetic messages it falls back to a plain SendMedia (no quote context).
func (c *Client) ReplyMedia(ctx context.Context, msg IncomingMessage, data []byte, mimeType, filename string) error {
	if msg.Synthetic {
		return c.SendMedia(ctx, msg.Chat, data, mimeType, filename)
	}
	return c.sendMedia(ctx, msg.Chat, data, mimeType, filename, c.quoteContext(msg))
}

// SendMedia sends media to a JID without quoting
func (c *Client) SendMedia(ctx context.Context, to types.JID, data []byte, mimeType, filename string) error {
	return c.sendMedia(ctx, normalizeJID(to), data, mimeType, filename, nil)
}

// DeleteMessage deletes (revokes) a message for everyone. The revoke carries
// the original sender, so the bot can delete other members' messages in groups
// where it is admin.
func (c *Client) DeleteMessage(ctx context.Context, msg IncomingMessage) error {
	_, err := c.cli.SendMessage(ctx, msg.Chat, c.revokeMessage(msg))
	if err != nil {
		return fmt.Errorf("failed to revoke message: %w", err)
	}
	return nil
}

// revokeMessage builds the revocation for an incoming message, carrying its
// original sender
func (c *Client) revokeMessage(msg IncomingMessage) *waE2E.Message {
	return c.cli.BuildRevoke(msg.Chat, msg.Sender, msg.ID)
}

// DownloadMedia downloads the document attachment from an incoming message.
// It returns an error if the message does not contain a document.
func (c *Client) DownloadMedia(ctx context.Context, msg IncomingMessage) ([]byte, error) {
	if msg.RawMessage == nil {
		return nil, fmt.Errorf("message has no raw data")
	}
	doc := msg.RawMessage.GetDocumentMessage()
	if doc == nil {
		return nil, fmt.Errorf("message does not contain a document")
	}
	data, err := c.cli.Download(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("failed to download document: %w", err)
	}
	return data, nil
}

// Group represents a WhatsApp group chat.
type Group struct {
	JID  types.JID
	Name string
}

// GroupParticipant represents a member of a WhatsApp group.
type GroupParticipant struct {
	JID          types.JID
	PhoneNumber  types.JID
	DisplayName  string
	IsAdmin      bool
	IsSuperAdmin bool
}

// GetJoinedGroups returns the WhatsApp groups the bot is participating in.
func (c *Client) GetJoinedGroups(ctx context.Context) ([]Group, error) {
	infos, err := c.cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get joined groups: %w", err)
	}

	groups := make([]Group, 0, len(infos))
	for _, info := range infos {
		groups = append(groups, Group{
			JID:  info.JID,
			Name: info.Name,
		})
	}
	return groups, nil
}

// GetGroupParticipants returns the participants of a WhatsApp group.
func (c *Client) GetGroupParticipants(ctx context.Context, groupJID types.JID) ([]GroupParticipant, error) {
	info, err := c.cli.GetGroupInfo(ctx, groupJID)
	if err != nil {
		return nil, fmt.Errorf("failed to get group info: %w", err)
	}

	participants := make([]GroupParticipant, 0, len(info.Participants))
	for _, p := range info.Participants {
		participants = append(participants, GroupParticipant{
			JID:          p.JID,
			PhoneNumber:  p.PhoneNumber,
			DisplayName:  p.DisplayName,
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	return participants, nil
}

// quoteContext builds the ContextInfo that makes a message a quote-reply
func (c *Client) quoteContext(msg IncomingMessage) *waE2E.ContextInfo {
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(msg.ID),
		Participant:   proto.String(msg.Sender.String()),
		QuotedMessage: msg.RawMessage,
	}
}

// sendMedia uploads media and sends it as image, audio or document depending on the mime type
func (c *Client) sendMedia(ctx context.Context, to types.JID, data []byte, mimeType, filename string, ctxInfo *waE2E.ContextInfo) error {
	mediaType := mediaTypeFor(mimeType)

	uploaded, err := c.cli.Upload(ctx, data, mediaType)
	if err != nil {
		return fmt.Errorf("failed to upload media: %w", err)
	}

	var out *waE2E.Message
	switch mediaType {
	case whatsmeow.MediaImage:
		out = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uploaded.FileLength),
			ContextInfo:   ctxInfo,
		}}
	case whatsmeow.MediaAudio:
		out = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uploaded.FileLength),
			ContextInfo:   ctxInfo,
		}}
	default:
		out = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Mimetype:      proto.String(mimeType),
			FileName:      proto.String(filename),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uploaded.FileLength),
			ContextInfo:   ctxInfo,
		}}
	}

	return c.send(ctx, to, out)
}

func (c *Client) send(ctx context.Context, to types.JID, msg *waE2E.Message) error {
	_, err := c.cli.SendMessage(ctx, to, msg)
	if err != nil {
		return fmt.Errorf("failed to send message to %s: %w", to.String(), err)
	}
	return nil
}

// normalizeJID converts legacy @c.us user JIDs to the @s.whatsapp.net server
// that whatsmeow requires for sending messages. Other JIDs are returned unchanged.
func normalizeJID(jid types.JID) types.JID {
	if jid.Server == "c.us" {
		return types.NewJID(jid.User, types.DefaultUserServer)
	}
	return jid
}

// mediaTypeFor maps a mime type to a whatsmeow media type
func mediaTypeFor(mimeType string) whatsmeow.MediaType {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return whatsmeow.MediaImage
	case strings.HasPrefix(mimeType, "audio/"):
		return whatsmeow.MediaAudio
	case strings.HasPrefix(mimeType, "video/"):
		return whatsmeow.MediaVideo
	default:
		return whatsmeow.MediaDocument
	}
}
