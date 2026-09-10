package whatsapp

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"whatsapp-mcp/paths"
	"whatsapp-mcp/storage"

	utilffmpeg "go.mau.fi/util/ffmpeg"
	"go.mau.fi/util/ffmpeg/waveform"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WebhookManager defines the interface for webhook emission.
type WebhookManager interface {
	EmitMessageEvent(msg storage.MessageWithNames) error
}

// Client wraps the WhatsApp client with additional functionality.
type Client struct {
	wa               *whatsmeow.Client
	store            *storage.MessageStore
	mediaStore       *storage.MediaStore
	webhookManager   WebhookManager // optional webhook manager
	mediaConfig      MediaConfig
	log              waLog.Logger
	logFile          *os.File
	historySyncChans map[string]chan bool // tracks pending sync requests by chat JID
	historySyncMux   sync.Mutex           // protects the map
	ctx              context.Context      // client lifecycle context
	cancel           context.CancelFunc   // cancel function to stop all goroutines
}

// fileLogger wraps a logger to write to both stdout and a file.
type fileLogger struct {
	base waLog.Logger
	file *os.File
}

// Errorf logs an error message to both stdout and file.
func (l *fileLogger) Errorf(msg string, args ...any) {
	l.base.Errorf(msg, args...)
	fmt.Fprintf(l.file, "[ERROR] "+msg+"\n", args...)
}

// Warnf logs a warning message to both stdout and file.
func (l *fileLogger) Warnf(msg string, args ...any) {
	l.base.Warnf(msg, args...)
	fmt.Fprintf(l.file, "[WARN] "+msg+"\n", args...)
}

// Infof logs an info message to both stdout and file.
func (l *fileLogger) Infof(msg string, args ...any) {
	l.base.Infof(msg, args...)
	fmt.Fprintf(l.file, "[INFO] "+msg+"\n", args...)
}

// Debugf logs a debug message to both stdout and file.
func (l *fileLogger) Debugf(msg string, args ...any) {
	l.base.Debugf(msg, args...)
	fmt.Fprintf(l.file, "[DEBUG] "+msg+"\n", args...)
}

// Sub creates a sub-logger for a specific module.
func (l *fileLogger) Sub(module string) waLog.Logger {
	return &fileLogger{
		base: l.base.Sub(module),
		file: l.file,
	}
}

// NewClient creates a new WhatsApp client with the given configuration.
func NewClient(store *storage.MessageStore, mediaStore *storage.MediaStore, webhookManager WebhookManager, logLevel string) (*Client, error) {
	// validate log level, default to INFO if invalid
	validLevels := map[string]bool{
		"DEBUG": true,
		"INFO":  true,
		"WARN":  true,
		"ERROR": true,
	}
	if !validLevels[logLevel] {
		logLevel = "INFO"
	}

	// create log file in data directory
	logFile, err := os.OpenFile(paths.WhatsAppLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// create base logger for stdout
	baseLogger := waLog.Stdout("whatsapp", logLevel, true)

	// Wrap with file logger
	logger := &fileLogger{
		base: baseLogger,
		file: logFile,
	}

	logger.Infof("Initializing WhatsApp client with log level: %s (logging to %s)", logLevel, paths.WhatsAppLogPath)

	// Load media configuration
	mediaConfig := LoadMediaConfig()
	logger.Infof("Media auto-download: enabled=%v, max_size=%d MB, types=%v",
		mediaConfig.AutoDownloadEnabled,
		mediaConfig.AutoDownloadMaxSize/(1024*1024),
		getEnabledTypes(mediaConfig.AutoDownloadTypes))

	ctx := context.Background()

	container, err := sqlstore.New(ctx, "sqlite", "file:"+paths.WhatsAppAuthDBPath+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create sqlstore: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get  device: %w", err)
	}

	waClient := whatsmeow.NewClient(deviceStore, logger)

	// create client lifecycle context
	clientCtx, cancel := context.WithCancel(context.Background())

	client := &Client{
		wa:               waClient,
		store:            store,
		mediaStore:       mediaStore,
		webhookManager:   webhookManager,
		mediaConfig:      mediaConfig,
		log:              logger,
		logFile:          logFile,
		historySyncChans: make(map[string]chan bool),
		ctx:              clientCtx,
		cancel:           cancel,
	}

	// Configure retry receipt handler: when a recipient can't decrypt a message,
	// WhatsApp sends a retry receipt. This callback loads the original protobuf
	// from the database so the message can be re-encrypted and resent.
	waClient.GetMessageForRetry = func(requester, to types.JID, id types.MessageID) *waE2E.Message {
		protoBytes, err := store.GetMessageProto(string(id))
		if err != nil {
			logger.Warnf("Error loading message proto for retry %s: %v", id, err)
			return nil
		}
		if protoBytes == nil {
			logger.Debugf("No stored proto for retry %s", id)
			return nil
		}
		var msg waE2E.Message
		if err := proto.Unmarshal(protoBytes, &msg); err != nil {
			logger.Warnf("Error unmarshalling message proto for retry %s: %v", id, err)
			return nil
		}
		logger.Infof("Loaded message proto for retry %s", id)
		return &msg
	}

	waClient.AddEventHandler(client.eventHandler)

	return client, nil
}

// IsLoggedIn reports whether the client is logged in.
func (c *Client) IsLoggedIn() bool {
	return c.wa.Store.ID != nil
}

// Connect establishes a connection to WhatsApp.
func (c *Client) Connect() error {
	return c.wa.Connect()
}

// Disconnect closes the WhatsApp connection and cleans up resources.
func (c *Client) Disconnect() {
	// cancel context to stop all running goroutines
	if c.cancel != nil {
		c.cancel()
	}
	c.wa.Disconnect()
	if c.logFile != nil {
		if err := c.logFile.Close(); err != nil {
			c.log.Errorf("failed to close log file: %v", err)
		}
	}
}

// GetQRChannel returns a channel for receiving QR codes for authentication.
func (c *Client) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	if c.IsLoggedIn() {
		return nil, fmt.Errorf("already logged in")
	}

	qrChan, err := c.wa.GetQRChannel(ctx)
	if err != nil {
		return nil, err
	}

	go func() {
		err := c.Connect()
		if err != nil {
			c.log.Errorf("failed to connect: %v", err)
		}
	}()

	return qrChan, nil
}

// SendTextMessage sends a text message to a chat.
// If replyToID is non-empty, the message is sent as a reply linked to that message.
func (c *Client) SendTextMessage(ctx context.Context, chatJID string, text string, replyToID string) error {
	targetJID, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}

	var msg *waE2E.Message

	if replyToID != "" {
		// Build a quoted reply message
		quotedMsg, err := c.store.GetMessageByID(replyToID)
		if err != nil {
			return fmt.Errorf("failed to look up quoted message: %w", err)
		}
		if quotedMsg == nil {
			return fmt.Errorf("quoted message %s not found in database", replyToID)
		}

		participant := quotedMsg.SenderJID

		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(text),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:      proto.String(replyToID),
					Participant:   proto.String(participant),
					QuotedMessage: &waE2E.Message{Conversation: proto.String(quotedMsg.Text)},
				},
			},
		}
	} else {
		msg = &waE2E.Message{
			Conversation: proto.String(text),
		}
	}

	resp, err := c.wa.SendMessage(ctx, targetJID, msg)
	if err != nil {
		return err
	}

	// Add to recent messages cache for retry receipt handling
	if err := c.wa.DangerousInternals().AddRecentMessage(ctx, targetJID, resp.ID, msg, nil); err != nil {
		c.log.Warnf("Failed to cache recently sent message %s: %v", resp.ID, err)
	}

	c.store.SaveMessage(storage.Message{
		ID:          resp.ID,
		ChatJID:     chatJID,
		SenderJID:   resp.Sender.String(),
		Text:        text,
		Timestamp:   resp.Timestamp,
		IsFromMe:    true,
		MessageType: "text",
	})

	// Persist protobuf for retry receipt handling across restarts
	if protoBytes, err := proto.Marshal(msg); err == nil {
		c.store.SaveMessageProto(resp.ID, protoBytes)
	} else {
		c.log.Warnf("Failed to marshal message proto for %s: %v", resp.ID, err)
	}

	return nil
}

// parseAddressableJID parses a JID that must address a chat or a user.
// types.ParseJID accepts a bare string as a server-only JID, so "not a jid"
// would parse cleanly and then be sent nowhere; requiring the user@server
// shape turns that into an error the caller can read.
func parseAddressableJID(jid string) (types.JID, error) {
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return types.EmptyJID, err
	}
	if parsed.User == "" || parsed.Server == "" {
		return types.EmptyJID, fmt.Errorf("expected a JID of the form user@server")
	}
	return parsed, nil
}

// resolveReactionTarget works out which chat a reaction goes to and which
// participant owns the message it hangs off.
//
// stored is the target message as the local history knows it, or nil when it
// predates that history; chatJID and senderJID are caller overrides. The
// returned sender is EmptyJID for our own messages, which is how
// BuildMessageKey encodes "from me" — passing our own JID instead would be
// equivalent, but keeping it empty also covers DMs, where the participant is
// implicit.
func resolveReactionTarget(stored *storage.Message, messageID string, chatJID string, senderJID string) (types.JID, types.JID, error) {
	if messageID == "" {
		return types.EmptyJID, types.EmptyJID, fmt.Errorf("message id is required")
	}

	if stored != nil {
		if chatJID == "" {
			chatJID = stored.ChatJID
		}
		if senderJID == "" && !stored.IsFromMe {
			senderJID = stored.SenderJID
		}
	}
	if chatJID == "" {
		return types.EmptyJID, types.EmptyJID, fmt.Errorf("message %s is not in the local history: pass chat_jid explicitly", messageID)
	}

	targetJID, err := parseAddressableJID(chatJID)
	if err != nil {
		return types.EmptyJID, types.EmptyJID, fmt.Errorf("invalid chat JID %q: %w", chatJID, err)
	}

	sender := types.EmptyJID
	if senderJID != "" {
		sender, err = parseAddressableJID(senderJID)
		if err != nil {
			return types.EmptyJID, types.EmptyJID, fmt.Errorf("invalid sender JID %q: %w", senderJID, err)
		}
	}

	return targetJID, sender, nil
}

// SendReaction attaches an emoji reaction to an existing message, the same way
// press-and-hold on a bubble does in the app. Passing an empty emoji removes a
// reaction this account sent earlier.
//
// chatJID and senderJID are resolved from the local message store when the
// target message is known there, which is the case for anything this daemon
// has seen. They only need to be supplied explicitly for a message that is
// older than the local history.
func (c *Client) SendReaction(ctx context.Context, chatJID string, messageID string, emoji string, senderJID string) error {
	var stored *storage.Message
	if messageID != "" {
		var err error
		stored, err = c.store.GetMessageByID(messageID)
		if err != nil {
			return fmt.Errorf("failed to look up target message: %w", err)
		}
	}

	targetJID, sender, err := resolveReactionTarget(stored, messageID, chatJID, senderJID)
	if err != nil {
		return err
	}
	chatJID = targetJID.String()

	msg := c.wa.BuildReaction(targetJID, sender, messageID, emoji)

	resp, err := c.wa.SendMessage(ctx, targetJID, msg)
	if err != nil {
		return err
	}

	// Add to recent messages cache for retry receipt handling
	if err := c.wa.DangerousInternals().AddRecentMessage(ctx, targetJID, resp.ID, msg, nil); err != nil {
		c.log.Warnf("Failed to cache recently sent reaction %s: %v", resp.ID, err)
	}

	// Persist it the same shape as an incoming reaction: emoji as text, type
	// "reaction" and reply_to_id pointing at the message it hangs off.
	c.store.SaveMessage(storage.Message{
		ID:          resp.ID,
		ChatJID:     chatJID,
		SenderJID:   resp.Sender.String(),
		Text:        emoji,
		Timestamp:   resp.Timestamp,
		IsFromMe:    true,
		MessageType: "reaction",
		ReplyToID:   messageID,
	})

	// Persist protobuf for retry receipt handling across restarts
	if protoBytes, err := proto.Marshal(msg); err == nil {
		c.store.SaveMessageProto(resp.ID, protoBytes)
	} else {
		c.log.Warnf("Failed to marshal reaction proto for %s: %v", resp.ID, err)
	}

	return nil
}

// mediaSendTimeout bounds the whole media send path: read, upload and send.
// Without it a stalled WhatsApp media connection blocks the caller forever.
// whatsmeow renews its media_conn token over the websocket, and while the
// socket is flapping that renewal never returns, so Upload never comes back.
const mediaSendTimeout = 90 * time.Second

// mediaHTTPClient downloads remote media. http.DefaultClient has no timeout,
// so an unreachable host would hang the request indefinitely.
var mediaHTTPClient = &http.Client{Timeout: 60 * time.Second}

// socketWaitTimeout is how long a media send tolerates a reconnecting socket
// before giving up with an actionable error instead of stalling.
const socketWaitTimeout = 10 * time.Second

// waitForSocket blocks until the WhatsApp websocket is usable again, or fails
// fast. IsLoggedIn only reports that a session exists, not that the socket is
// alive, so it is not enough on its own: uploads renew a media_conn token over
// that socket and starting one while it is down stalls until the caller gives
// up, which is what made a flapping connection look like a client timeout.
func (c *Client) waitForSocket() error {
	if c.wa.WaitForConnection(socketWaitTimeout) {
		return nil
	}
	return fmt.Errorf("WhatsApp socket is not connected (reconnecting); retry in a few seconds")
}

// readMediaSource reads media data from a local file path or an http(s) URL.
// Returns the data bytes and MIME type.
//
// Only "http://" and "https://" are treated as remote. Everything else is a
// local path: absolute, "~/"-relative, "file://" or relative to the process
// working directory. A relative path that does not exist is rejected outright
// instead of being handed to the HTTP client as if it were a hostname.
//
// ctx bounds the remote download so the caller's deadline wins over
// mediaHTTPClient's own timeout.
func (c *Client) readMediaSource(ctx context.Context, source string) ([]byte, string, error) {
	const maxSize = 16 * 1024 * 1024

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		c.log.Infof("Downloading from URL: %s", source)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, "", fmt.Errorf("failed to build request: %w", err)
		}
		resp, err := mediaHTTPClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("failed to download: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("failed to download: HTTP %d", resp.StatusCode)
		}

		limitedReader := io.LimitReader(resp.Body, int64(maxSize)+1)
		data, err := io.ReadAll(limitedReader)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read data: %w", err)
		}
		if len(data) > maxSize {
			return nil, "", fmt.Errorf("file too large (max 16MB)")
		}

		mimeType := resp.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		return data, mimeType, nil
	}

	path := strings.TrimPrefix(source, "file://")
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("failed to expand home directory: %w", err)
		}
		path = home + path[1:]
	}
	if !filepath.IsAbs(path) {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			return nil, "", fmt.Errorf("invalid file path %q: %w", source, absErr)
		}
		if _, statErr := os.Stat(abs); statErr != nil {
			return nil, "", fmt.Errorf("%q is neither an http(s) URL nor an existing file: pass an absolute path", source)
		}
		path = abs
	}

	// Stat before reading: a directory, a socket or a character device such as
	// /dev/zero would otherwise be slurped into memory (or block forever)
	// before the size check downstream ever ran.
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read local file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() > maxSize {
		return nil, "", fmt.Errorf("file too large (max 16MB)")
	}

	c.log.Infof("Reading local file: %s", path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read local file: %w", err)
	}
	if len(data) > maxSize {
		return nil, "", fmt.Errorf("file too large (max 16MB)")
	}
	return data, http.DetectContentType(data), nil
}

// mediaExecutable returns a common absolute path when LaunchAgents cannot see
// Homebrew binaries, and otherwise falls back to PATH resolution.
func mediaExecutable(name string) string {
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return name
}

// convertGifToMp4 converts a GIF file to MP4 using ffmpeg.
// Returns the MP4 data or an error if ffmpeg is not available or conversion fails.
// ctx bounds the ffmpeg run so a stalled conversion cannot outlive the send.
func convertGifToMp4(ctx context.Context, gifData []byte) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "wa-gif-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	gifPath := filepath.Join(tmpDir, "input.gif")
	mp4Path := filepath.Join(tmpDir, "output.mp4")

	if err := os.WriteFile(gifPath, gifData, 0644); err != nil {
		return nil, fmt.Errorf("failed to write temp gif: %w", err)
	}

	cmd := exec.CommandContext(ctx, mediaExecutable("ffmpeg"), "-y", "-i", gifPath,
		"-movflags", "faststart",
		"-pix_fmt", "yuv420p",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-an", mp4Path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion failed: %w\n%s", err, string(output))
	}

	return os.ReadFile(mp4Path)
}

// fetchMP4Fallback downloads the MP4 twin of a remote GIF (Giphy serves one).
// The request carries ctx so the caller's deadline bounds it.
func (c *Client) fetchMP4Fallback(ctx context.Context, mp4URL string, maxSize int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mp4URL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid MP4 URL: %w", err)
	}

	resp, err := mediaHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxSize)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("MP4 version too large (max %d bytes)", maxSize)
	}
	return data, nil
}

// convertAudioToVoiceNote transcodes arbitrary audio into the strict format
// required by WhatsApp mobile clients and returns duration and waveform data.
func convertAudioToVoiceNote(ctx context.Context, audioData []byte) ([]byte, uint32, []byte, error) {
	const maxVoiceNoteSize = 16 * 1024 * 1024

	tmpDir, err := os.MkdirTemp("", "wa-voice-*")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	inputPath := filepath.Join(tmpDir, "input")
	voicePath := filepath.Join(tmpDir, "voice.ogg")
	if err := os.WriteFile(inputPath, audioData, 0600); err != nil {
		return nil, 0, nil, fmt.Errorf("failed to write temporary audio: %w", err)
	}

	cmd := exec.CommandContext(ctx, mediaExecutable("ffmpeg"), "-y", "-i", inputPath,
		"-vn",
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
		"-c:a", "libopus",
		"-application", "voip",
		"-b:a", "32k",
		"-vbr", "on",
		"-compression_level", "10",
		"-ac", "1",
		"-ar", "16000",
		"-f", "ogg",
		voicePath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, 0, nil, fmt.Errorf("ffmpeg voice-note conversion failed: %w\n%s", err, string(output))
	}

	voiceData, err := os.ReadFile(voicePath)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to read converted voice note: %w", err)
	}
	if len(voiceData) > maxVoiceNoteSize {
		return nil, 0, nil, fmt.Errorf("converted voice note is too large (max 16MB)")
	}

	utilffmpeg.SetPath(mediaExecutable("ffmpeg"))
	waveformValues, err := waveform.Generate(ctx, voicePath, 64, 100)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to generate voice-note waveform: %w", err)
	}
	waveformData := make([]byte, len(waveformValues))
	for i, value := range waveformValues {
		waveformData[i] = byte(value)
	}

	durationOutput, err := exec.CommandContext(
		ctx,
		mediaExecutable("ffprobe"),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		voicePath,
	).CombinedOutput()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("ffprobe duration detection failed: %w\n%s", err, string(durationOutput))
	}

	duration, err := strconv.ParseFloat(strings.TrimSpace(string(durationOutput)), 64)
	if err != nil || duration <= 0 {
		return nil, 0, nil, fmt.Errorf("invalid converted audio duration %q", strings.TrimSpace(string(durationOutput)))
	}
	seconds := uint32(math.Round(duration))
	if seconds == 0 {
		seconds = 1
	}

	return voiceData, seconds, waveformData, nil
}

// SendImageMessage sends an image or GIF to a WhatsApp chat.
// imageSource can be a URL (http/https) or a local file path (absolute or ~/...).
// For GIF sources, it sends as a video with GifPlayback=true (WhatsApp requirement).
func (c *Client) SendImageMessage(ctx context.Context, chatJID string, imageSource string, caption string, replyToID string) (err error) {
	c.log.Infof("SendImageMessage called: chatJID=%s, imageSource=%s, caption=%s", chatJID, imageSource, caption)
	defer func() {
		if err != nil {
			c.log.Errorf("SendImageMessage failed: chatJID=%s, imageSource=%s: %v", chatJID, imageSource, err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, mediaSendTimeout)
	defer cancel()

	if err = c.waitForSocket(); err != nil {
		return err
	}

	targetJID, err := types.ParseJID(chatJID)
	if err != nil {
		c.log.Errorf("SendImageMessage: invalid chat JID: %v", err)
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	data, mimeType, err := c.readMediaSource(ctx, imageSource)
	if err != nil {
		return fmt.Errorf("failed to read image: %w", err)
	}

	isGif := strings.Contains(mimeType, "gif") || strings.HasSuffix(strings.ToLower(imageSource), ".gif")

	// For GIFs: WhatsApp requires MP4 data with GifPlayback flag.
	// If we got a .gif file from a URL, try to fetch the .mp4 version (Giphy provides this).
	const maxMediaSize = 16 * 1024 * 1024
	if isGif && strings.Contains(mimeType, "gif") && !strings.HasPrefix(imageSource, "/") {
		mp4URL := strings.Replace(imageSource, "/giphy.gif", "/giphy.mp4", 1)
		mp4URL = strings.Replace(mp4URL, "rid=giphy.gif", "rid=giphy.mp4", 1)
		if mp4URL != imageSource {
			c.log.Infof("GIF detected, fetching MP4 version: %s", mp4URL)
			if mp4Data, mp4Err := c.fetchMP4Fallback(ctx, mp4URL, maxMediaSize); mp4Err != nil {
				c.log.Warnf("GIF MP4 fallback failed: %v (falling back to ffmpeg)", mp4Err)
			} else if len(mp4Data) > 0 {
				data = mp4Data
				mimeType = "video/mp4"
				c.log.Infof("Using MP4 version (%d bytes)", len(data))
			}
		}
	}

	// For local GIFs (or URL GIFs where MP4 fetch failed), convert to MP4 with ffmpeg
	if isGif && strings.Contains(mimeType, "gif") {
		c.log.Infof("Converting GIF to MP4 with ffmpeg...")
		mp4Data, err := convertGifToMp4(ctx, data)
		if err != nil {
			c.log.Warnf("ffmpeg GIF conversion failed: %v (sending raw GIF)", err)
		} else {
			data = mp4Data
			mimeType = "video/mp4"
			c.log.Infof("GIF converted to MP4 (%d bytes)", len(data))
		}
	}

	var msg *waE2E.Message

	if isGif {
		// GIFs must be sent as video with GifPlayback flag
		uploaded, err := c.wa.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return fmt.Errorf("failed to upload GIF: %w", err)
		}

		videoMsg := &waE2E.VideoMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			Mimetype:      proto.String(mimeType),
			GifPlayback:   proto.Bool(true),
		}
		if caption != "" {
			videoMsg.Caption = proto.String(caption)
		}

		msg = &waE2E.Message{VideoMessage: videoMsg}
	} else {
		// Regular image
		uploaded, err := c.wa.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return fmt.Errorf("failed to upload image: %w", err)
		}

		imageMsg := &waE2E.ImageMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			Mimetype:      proto.String(mimeType),
		}
		if caption != "" {
			imageMsg.Caption = proto.String(caption)
		}

		msg = &waE2E.Message{ImageMessage: imageMsg}
	}

	// Add reply context if specified
	if replyToID != "" {
		quotedMsg, err := c.store.GetMessageByID(replyToID)
		if err != nil {
			return fmt.Errorf("failed to look up quoted message: %w", err)
		}
		if quotedMsg == nil {
			return fmt.Errorf("quoted message %s not found in database", replyToID)
		}

		contextInfo := &waE2E.ContextInfo{
			StanzaID:      proto.String(replyToID),
			Participant:   proto.String(quotedMsg.SenderJID),
			QuotedMessage: &waE2E.Message{Conversation: proto.String(quotedMsg.Text)},
		}

		if isGif {
			msg.VideoMessage.ContextInfo = contextInfo
		} else {
			msg.ImageMessage.ContextInfo = contextInfo
		}
	}

	c.log.Infof("Sending image message to %s...", chatJID)
	sendResp, err := c.wa.SendMessage(ctx, targetJID, msg)
	if err != nil {
		c.log.Errorf("SendImageMessage: failed to send: %v", err)
		return fmt.Errorf("failed to send image: %w", err)
	}

	// Add to recent messages cache for retry receipt handling
	if err := c.wa.DangerousInternals().AddRecentMessage(ctx, targetJID, sendResp.ID, msg, nil); err != nil {
		c.log.Warnf("Failed to cache recently sent message %s: %v", sendResp.ID, err)
	}

	c.log.Infof("Image sent successfully! ID=%s", sendResp.ID)
	// Save to DB
	msgType := "image"
	if isGif {
		msgType = "gif"
	}
	text := caption
	if text == "" {
		text = "[" + msgType + "]"
	}

	c.store.SaveMessage(storage.Message{
		ID:          sendResp.ID,
		ChatJID:     chatJID,
		SenderJID:   sendResp.Sender.String(),
		Text:        text,
		Timestamp:   sendResp.Timestamp,
		IsFromMe:    true,
		MessageType: msgType,
		ReplyToID:   replyToID,
	})

	// Persist protobuf for retry receipt handling across restarts
	if protoBytes, err := proto.Marshal(msg); err == nil {
		c.store.SaveMessageProto(sendResp.ID, protoBytes)
	} else {
		c.log.Warnf("Failed to marshal message proto for %s: %v", sendResp.ID, err)
	}

	return nil
}

// SendVideoMessage sends a video to a WhatsApp chat.
// videoSource can be a URL (http/https) or a local file path (absolute or ~/...).
func (c *Client) SendVideoMessage(ctx context.Context, chatJID string, videoSource string, caption string, replyToID string) (err error) {
	c.log.Infof("SendVideoMessage called: chatJID=%s, videoSource=%s, caption=%s", chatJID, videoSource, caption)
	defer func() {
		if err != nil {
			c.log.Errorf("SendVideoMessage failed: chatJID=%s, videoSource=%s: %v", chatJID, videoSource, err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, mediaSendTimeout)
	defer cancel()

	if err = c.waitForSocket(); err != nil {
		return err
	}

	targetJID, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	data, mimeType, err := c.readMediaSource(ctx, videoSource)
	if err != nil {
		return fmt.Errorf("failed to read video: %w", err)
	}

	// Upload as video
	uploaded, err := c.wa.Upload(ctx, data, whatsmeow.MediaVideo)
	if err != nil {
		return fmt.Errorf("failed to upload video: %w", err)
	}

	videoMsg := &waE2E.VideoMessage{
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(data))),
		Mimetype:      proto.String(mimeType),
	}
	if caption != "" {
		videoMsg.Caption = proto.String(caption)
	}

	msg := &waE2E.Message{VideoMessage: videoMsg}

	// Add reply context if specified
	if replyToID != "" {
		quotedMsg, err := c.store.GetMessageByID(replyToID)
		if err != nil {
			return fmt.Errorf("failed to look up quoted message: %w", err)
		}
		if quotedMsg == nil {
			return fmt.Errorf("quoted message %s not found in database", replyToID)
		}
		msg.VideoMessage.ContextInfo = &waE2E.ContextInfo{
			StanzaID:      proto.String(replyToID),
			Participant:   proto.String(quotedMsg.SenderJID),
			QuotedMessage: &waE2E.Message{Conversation: proto.String(quotedMsg.Text)},
		}
	}

	c.log.Infof("Sending video message to %s...", chatJID)
	sendResp, err := c.wa.SendMessage(ctx, targetJID, msg)
	if err != nil {
		return fmt.Errorf("failed to send video: %w", err)
	}

	// Add to recent messages cache for retry receipt handling
	if err := c.wa.DangerousInternals().AddRecentMessage(ctx, targetJID, sendResp.ID, msg, nil); err != nil {
		c.log.Warnf("Failed to cache recently sent message %s: %v", sendResp.ID, err)
	}

	c.log.Infof("Video sent successfully! ID=%s", sendResp.ID)
	text := caption
	if text == "" {
		text = "[video]"
	}

	c.store.SaveMessage(storage.Message{
		ID:          sendResp.ID,
		ChatJID:     chatJID,
		SenderJID:   sendResp.Sender.String(),
		Text:        text,
		Timestamp:   sendResp.Timestamp,
		IsFromMe:    true,
		MessageType: "video",
		ReplyToID:   replyToID,
	})

	// Persist protobuf for retry receipt handling across restarts
	if protoBytes, err := proto.Marshal(msg); err == nil {
		c.store.SaveMessageProto(sendResp.ID, protoBytes)
	} else {
		c.log.Warnf("Failed to marshal message proto for %s: %v", sendResp.ID, err)
	}

	return nil
}

// SendVoiceMessage converts an audio source to Ogg/Opus and sends it as a
// WhatsApp push-to-talk voice note.
func (c *Client) SendVoiceMessage(ctx context.Context, chatJID string, audioSource string, replyToID string) (err error) {
	c.log.Infof("SendVoiceMessage called: chatJID=%s, audioSource=%s", chatJID, audioSource)
	defer func() {
		if err != nil {
			c.log.Errorf("SendVoiceMessage failed: chatJID=%s, audioSource=%s: %v", chatJID, audioSource, err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, mediaSendTimeout)
	defer cancel()

	if err = c.waitForSocket(); err != nil {
		return err
	}

	targetJID, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	audioData, _, err := c.readMediaSource(ctx, audioSource)
	if err != nil {
		return fmt.Errorf("failed to read audio: %w", err)
	}

	voiceData, duration, waveformData, err := convertAudioToVoiceNote(ctx, audioData)
	if err != nil {
		return err
	}

	uploaded, err := c.wa.Upload(ctx, voiceData, whatsmeow.MediaAudio)
	if err != nil {
		return fmt.Errorf("failed to upload voice note: %w", err)
	}

	audioMsg := &waE2E.AudioMessage{
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(voiceData))),
		Mimetype:      proto.String("audio/ogg; codecs=opus"),
		Seconds:       proto.Uint32(duration),
		Waveform:      waveformData,
		PTT:           proto.Bool(true),
	}
	msg := &waE2E.Message{AudioMessage: audioMsg}

	if replyToID != "" {
		quotedMsg, err := c.store.GetMessageByID(replyToID)
		if err != nil {
			return fmt.Errorf("failed to look up quoted message: %w", err)
		}
		if quotedMsg == nil {
			return fmt.Errorf("quoted message %s not found in database", replyToID)
		}
		audioMsg.ContextInfo = &waE2E.ContextInfo{
			StanzaID:      proto.String(replyToID),
			Participant:   proto.String(quotedMsg.SenderJID),
			QuotedMessage: &waE2E.Message{Conversation: proto.String(quotedMsg.Text)},
		}
	}

	sendResp, err := c.wa.SendMessage(ctx, targetJID, msg)
	if err != nil {
		return fmt.Errorf("failed to send voice note: %w", err)
	}

	if err := c.wa.DangerousInternals().AddRecentMessage(ctx, targetJID, sendResp.ID, msg, nil); err != nil {
		c.log.Warnf("Failed to cache recently sent message %s: %v", sendResp.ID, err)
	}

	c.store.SaveMessage(storage.Message{
		ID:          sendResp.ID,
		ChatJID:     chatJID,
		SenderJID:   sendResp.Sender.String(),
		Text:        "[Audio]",
		Timestamp:   sendResp.Timestamp,
		IsFromMe:    true,
		MessageType: "ptt",
		ReplyToID:   replyToID,
	})

	if protoBytes, err := proto.Marshal(msg); err == nil {
		c.store.SaveMessageProto(sendResp.ID, protoBytes)
	} else {
		c.log.Warnf("Failed to marshal message proto for %s: %v", sendResp.ID, err)
	}

	c.log.Infof("Voice note sent successfully! ID=%s", sendResp.ID)
	return nil
}

// RequestHistorySync requests additional message history from WhatsApp.
// If waitForSync is true, it blocks until the sync completes and returns the new messages.
func (c *Client) RequestHistorySync(ctx context.Context, chatJID string, count int, waitForSync bool) ([]storage.MessageWithNames, error) {
	// parse the chatJID string to types.JID
	parsedJID, err := types.ParseJID(chatJID)
	if err != nil {
		return nil, fmt.Errorf("invalid chat JID: %w", err)
	}

	normalizedJID := c.normalizeJID(parsedJID)

	oldestMessage, err := c.store.GetOldestMessage(normalizedJID)
	if err != nil {
		return nil, fmt.Errorf("failed to get oldest message: %w", err)
	}

	if oldestMessage == nil {
		return nil, fmt.Errorf("no messages in database for this chat. Please wait for initial history sync")
	}

	lastKnownMessageInfo := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     parsedJID,
			IsFromMe: oldestMessage.IsFromMe,
		},
		ID:        oldestMessage.ID,
		Timestamp: oldestMessage.Timestamp,
	}

	reqMsg := c.wa.BuildHistorySyncRequest(lastKnownMessageInfo, count)

	if waitForSync {
		oldestTimestamp := oldestMessage.Timestamp

		syncChan := make(chan bool, 1)

		c.historySyncMux.Lock()
		c.historySyncChans[normalizedJID] = syncChan
		c.historySyncMux.Unlock()

		_, err = c.wa.SendMessage(ctx, c.wa.Store.ID.ToNonAD(), reqMsg, whatsmeow.SendRequestExtra{Peer: true})
		if err != nil {
			// clean up the channel on error
			c.historySyncMux.Lock()
			delete(c.historySyncChans, normalizedJID)
			c.historySyncMux.Unlock()
			return nil, fmt.Errorf("failed to send history sync request: %w", err)
		}

		c.log.Infof("Sent ON_DEMAND history sync request for chat %s (count: %d)", normalizedJID, count)

		// wait for signal with timeout (30 seconds)
		select {
		case <-syncChan:
			c.log.Debugf("History sync completed for chat %s", normalizedJID)
		case <-time.After(30 * time.Second):
			// clean up on timeout
			c.historySyncMux.Lock()
			delete(c.historySyncChans, normalizedJID)
			c.historySyncMux.Unlock()
			return nil, fmt.Errorf("timeout waiting for history sync. Try using wait_for_sync=false for async mode")
		}

		// retrieve newly loaded messages from database
		messages, err := c.store.GetChatMessagesOlderThan(normalizedJID, oldestTimestamp, count)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve newly loaded messages: %w", err)
		}

		c.log.Infof("Retrieved %d newly loaded messages for chat %s", len(messages), normalizedJID)
		return messages, nil
	} else {
		// asynchronous mode - send request and return immediately
		_, err = c.wa.SendMessage(ctx, c.wa.Store.ID.ToNonAD(), reqMsg, whatsmeow.SendRequestExtra{Peer: true})
		if err != nil {
			return nil, fmt.Errorf("failed to send history sync request: %w", err)
		}

		c.log.Infof("Sent ON_DEMAND history sync request for chat %s (count: %d, async mode)", normalizedJID, count)
		return []storage.MessageWithNames{}, nil
	}
}

// MyInfo contains the user's own WhatsApp profile information
type MyInfo struct {
	JID          string // User's WhatsApp JID
	PushName     string // User's display name (from store)
	Status       string // User's bio/status message
	PictureID    string // Profile picture ID
	PictureURL   string // Profile picture download URL (empty if not set)
	BusinessName string // Verified business name (if applicable)
}

// GetMyInfo retrieves the current user's WhatsApp profile information
func (c *Client) GetMyInfo(ctx context.Context) (*MyInfo, error) {
	if !c.IsLoggedIn() {
		return nil, fmt.Errorf("not logged in")
	}

	myJID := c.wa.Store.ID.ToNonAD()

	// Get basic user info (status, picture ID, verified business name)
	userInfoMap, err := c.wa.GetUserInfo(ctx, []types.JID{myJID})
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}

	userInfo, ok := userInfoMap[myJID]
	if !ok {
		return nil, fmt.Errorf("user info not found for own JID")
	}

	// Get push name from store
	pushName := c.wa.Store.PushName

	// Get contact info for business name (if available)
	var businessName string
	if c.wa.Store.Contacts != nil {
		contactInfo, err := c.wa.Store.Contacts.GetContact(ctx, myJID)
		if err == nil && contactInfo.Found {
			businessName = contactInfo.BusinessName
		}
	}

	// Try to get profile picture URL
	var pictureURL string
	picInfo, err := c.wa.GetProfilePictureInfo(ctx, myJID, &whatsmeow.GetProfilePictureParams{
		Preview: false,
	})
	if err == nil && picInfo != nil {
		pictureURL = picInfo.URL
	}
	// Ignore ErrProfilePictureNotSet and ErrProfilePictureUnauthorized - just leave URL empty

	return &MyInfo{
		JID:          myJID.String(),
		PushName:     pushName,
		Status:       userInfo.Status,
		PictureID:    userInfo.PictureID,
		PictureURL:   pictureURL,
		BusinessName: businessName,
	}, nil
}

// ── Community / Group management ─────────────────────────────────────────────

// CreateCommunity creates a WhatsApp community (parent group).
func (c *Client) CreateCommunity(ctx context.Context, name string) (*types.GroupInfo, error) {
	req := whatsmeow.ReqCreateGroup{Name: name}
	req.IsParent = true
	return c.wa.CreateGroup(ctx, req)
}

// CreateCommunityGroup creates a new group inside an existing community.
func (c *Client) CreateCommunityGroup(ctx context.Context, communityJID string, name string) (*types.GroupInfo, error) {
	parentJID, err := types.ParseJID(communityJID)
	if err != nil {
		return nil, fmt.Errorf("invalid community JID: %w", err)
	}
	req := whatsmeow.ReqCreateGroup{Name: name}
	req.LinkedParentJID = parentJID
	return c.wa.CreateGroup(ctx, req)
}

// ListCommunityGroups returns all sub-groups of a community.
func (c *Client) ListCommunityGroups(ctx context.Context, communityJID string) ([]*types.GroupLinkTarget, error) {
	parentJID, err := types.ParseJID(communityJID)
	if err != nil {
		return nil, fmt.Errorf("invalid community JID: %w", err)
	}
	return c.wa.GetSubGroups(ctx, parentJID)
}

// UnlinkGroupFromCommunity removes a group from a community.
func (c *Client) UnlinkGroupFromCommunity(ctx context.Context, communityJID string, groupJID string) error {
	parentJID, err := types.ParseJID(communityJID)
	if err != nil {
		return fmt.Errorf("invalid community JID: %w", err)
	}
	childJID, err := types.ParseJID(groupJID)
	if err != nil {
		return fmt.Errorf("invalid group JID: %w", err)
	}
	return c.wa.UnlinkGroup(ctx, parentJID, childJID)
}

// LinkGroupToCommunity adds an existing group to a community.
func (c *Client) LinkGroupToCommunity(ctx context.Context, communityJID string, groupJID string) error {
	parentJID, err := types.ParseJID(communityJID)
	if err != nil {
		return fmt.Errorf("invalid community JID: %w", err)
	}
	childJID, err := types.ParseJID(groupJID)
	if err != nil {
		return fmt.Errorf("invalid group JID: %w", err)
	}
	return c.wa.LinkGroup(ctx, parentJID, childJID)
}

// GetCommunityInfo returns info about a community (parent group).
func (c *Client) GetCommunityInfo(ctx context.Context, communityJID string) (*types.GroupInfo, error) {
	jid, err := types.ParseJID(communityJID)
	if err != nil {
		return nil, fmt.Errorf("invalid community JID: %w", err)
	}
	info, err := c.wa.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}
	if !info.IsParent {
		return nil, fmt.Errorf("JID %s is not a community (parent group)", communityJID)
	}
	return info, nil
}

// ProfilePicture contains profile picture info for a single JID.
type ProfilePicture struct {
	JID       string // The JID that was queried
	PictureID string // Profile picture ID (empty if not set)
	URL       string // Profile picture download URL (empty if not set or private)
	Error     string // Error message if the lookup failed
}

// GetProfilePicture retrieves the profile picture URL for a single JID.
func (c *Client) GetProfilePicture(ctx context.Context, jidStr string) (*ProfilePicture, error) {
	if !c.IsLoggedIn() {
		return nil, fmt.Errorf("not logged in")
	}

	targetJID, err := types.ParseJID(jidStr)
	if err != nil {
		return nil, fmt.Errorf("invalid JID: %w", err)
	}

	pic := &ProfilePicture{JID: jidStr}

	picInfo, err := c.wa.GetProfilePictureInfo(ctx, targetJID, &whatsmeow.GetProfilePictureParams{
		Preview: false,
	})
	if err != nil {
		// Not an error worth propagating — the picture is just unavailable
		pic.Error = err.Error()
		return pic, nil
	}
	if picInfo != nil {
		pic.PictureID = picInfo.ID
		pic.URL = picInfo.URL
	}

	return pic, nil
}

// GetProfilePictures retrieves profile picture URLs for multiple JIDs.
func (c *Client) GetProfilePictures(ctx context.Context, jidStrs []string) ([]ProfilePicture, error) {
	if !c.IsLoggedIn() {
		return nil, fmt.Errorf("not logged in")
	}

	results := make([]ProfilePicture, 0, len(jidStrs))
	for _, jidStr := range jidStrs {
		pic, err := c.GetProfilePicture(ctx, jidStr)
		if err != nil {
			results = append(results, ProfilePicture{
				JID:   jidStr,
				Error: err.Error(),
			})
			continue
		}
		results = append(results, *pic)
	}

	return results, nil
}

// getEnabledTypes returns a list of enabled media types for logging.
func getEnabledTypes(types map[string]bool) []string {
	var enabled []string
	for t, v := range types {
		if v {
			enabled = append(enabled, t)
		}
	}
	return enabled
}

// SendDocumentMessage sends a document/file to a WhatsApp chat.
// fileSource can be a URL (http/https) or a local file path (absolute or ~/...).
func (c *Client) SendDocumentMessage(ctx context.Context, chatJID string, fileSource string, fileName string, caption string, replyToID string) (err error) {
	c.log.Infof("SendDocumentMessage called: chatJID=%s, fileSource=%s, fileName=%s", chatJID, fileSource, fileName)
	defer func() {
		if err != nil {
			c.log.Errorf("SendDocumentMessage failed: chatJID=%s, fileSource=%s: %v", chatJID, fileSource, err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, mediaSendTimeout)
	defer cancel()

	if err = c.waitForSocket(); err != nil {
		return err
	}

	targetJID, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	data, mimeType, err := c.readMediaSource(ctx, fileSource)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Use provided fileName or derive from source path
	if fileName == "" {
		fileName = filepath.Base(fileSource)
		if fileName == "." || fileName == "" {
			fileName = "document"
		}
	}

	uploaded, err := c.wa.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Errorf("failed to upload document: %w", err)
	}

	docMsg := &waE2E.DocumentMessage{
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(data))),
		Mimetype:      proto.String(mimeType),
		FileName:      proto.String(fileName),
	}
	if caption != "" {
		docMsg.Caption = proto.String(caption)
	}

	msg := &waE2E.Message{DocumentMessage: docMsg}

	if replyToID != "" {
		quotedMsg, err := c.store.GetMessageByID(replyToID)
		if err != nil {
			return fmt.Errorf("failed to look up quoted message: %w", err)
		}
		if quotedMsg == nil {
			return fmt.Errorf("quoted message %s not found in database", replyToID)
		}
		msg.DocumentMessage.ContextInfo = &waE2E.ContextInfo{
			StanzaID:      proto.String(replyToID),
			Participant:   proto.String(quotedMsg.SenderJID),
			QuotedMessage: &waE2E.Message{Conversation: proto.String(quotedMsg.Text)},
		}
	}

	c.log.Infof("Sending document message to %s...", chatJID)
	sendResp, err := c.wa.SendMessage(ctx, targetJID, msg)
	if err != nil {
		return fmt.Errorf("failed to send document: %w", err)
	}

	if err := c.wa.DangerousInternals().AddRecentMessage(ctx, targetJID, sendResp.ID, msg, nil); err != nil {
		c.log.Warnf("Failed to cache recently sent message %s: %v", sendResp.ID, err)
	}

	c.log.Infof("Document sent successfully! ID=%s", sendResp.ID)
	text := caption
	if text == "" {
		text = "[" + fileName + "]"
	}

	c.store.SaveMessage(storage.Message{
		ID:          sendResp.ID,
		ChatJID:     chatJID,
		SenderJID:   sendResp.Sender.String(),
		Text:        text,
		Timestamp:   sendResp.Timestamp,
		IsFromMe:    true,
		MessageType: "document",
		ReplyToID:   replyToID,
	})

	if protoBytes, err := proto.Marshal(msg); err == nil {
		c.store.SaveMessageProto(sendResp.ID, protoBytes)
	} else {
		c.log.Warnf("Failed to marshal message proto for %s: %v", sendResp.ID, err)
	}

	return nil
}
