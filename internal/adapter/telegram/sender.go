package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// InlineButton is a single button in a Telegram inline keyboard.
type InlineButton struct {
	Text         string
	CallbackData string
}

// Sender sends messages and files to the Telegram Bot API.
type Sender struct {
	token   string
	baseAPI string
	client  *http.Client
}

// BotCommand represents a Telegram bot command menu item.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// NewSender creates a Telegram Sender.
func NewSender(token string) *Sender {
	return &Sender{
		token:   token,
		baseAPI: telegramAPI,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Sender) apiURL(method string) string {
	base := s.baseAPI
	if base == "" {
		base = telegramAPI
	}
	return fmt.Sprintf("%s%s/%s", base, s.token, method)
}

func (s *Sender) call(ctx context.Context, method string, payload interface{}) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL(method), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w, body: %s", err, string(raw))
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram error: %s", string(raw))
	}
	return result.Result, nil
}

func extractMsgID(raw json.RawMessage) (int, error) {
	var msg struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// SendText sends a plain text message.
func (s *Sender) SendText(ctx context.Context, chatID int64, text string, threadID ...int) (int, error) {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if len(threadID) > 0 && threadID[0] > 0 {
		payload["message_thread_id"] = threadID[0]
	}
	raw, err := s.call(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	return extractMsgID(raw)
}

// SendHTML sends an HTML-formatted message.
func (s *Sender) SendHTML(ctx context.Context, chatID int64, html string, threadID ...int) (int, error) {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       html,
		"parse_mode": "HTML",
	}
	if len(threadID) > 0 && threadID[0] > 0 {
		payload["message_thread_id"] = threadID[0]
	}
	raw, err := s.call(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	return extractMsgID(raw)
}

// SendHTMLToThread sends an HTML-formatted message to a specific topic/thread.
// If threadID is 0, behaves like SendHTML (sends to General / main chat).
func (s *Sender) SendHTMLToThread(ctx context.Context, chatID int64, threadID int, html string) (int, error) {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       html,
		"parse_mode": "HTML",
	}
	if threadID > 0 {
		payload["message_thread_id"] = threadID
	}
	raw, err := s.call(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	return extractMsgID(raw)
}

// SendWithKeyboard sends a message with inline keyboard.
func (s *Sender) SendWithKeyboard(ctx context.Context, chatID int64, text string, rows [][]InlineButton, threadID ...int) (int, error) {
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": buildKeyboard(rows),
	}
	if len(threadID) > 0 && threadID[0] > 0 {
		payload["message_thread_id"] = threadID[0]
	}
	raw, err := s.call(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	return extractMsgID(raw)
}

// EditMessage edits an existing message's text.
func (s *Sender) EditMessage(ctx context.Context, chatID int64, msgID int, text string) error {
	_, err := s.call(ctx, "editMessageText", map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
		"text":       text,
		"parse_mode": "HTML",
	})
	return err
}

// EditWithKeyboard edits text and keyboard.
func (s *Sender) EditWithKeyboard(ctx context.Context, chatID int64, msgID int, text string, rows [][]InlineButton) error {
	_, err := s.call(ctx, "editMessageText", map[string]interface{}{
		"chat_id":      chatID,
		"message_id":   msgID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": buildKeyboard(rows),
	})
	return err
}

// AnswerCallback acknowledges a callback query.
func (s *Sender) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	_, err := s.call(ctx, "answerCallbackQuery", map[string]interface{}{
		"callback_query_id": callbackID,
		"text":              text,
	})
	return err
}

// DeleteMessage removes a message.
func (s *Sender) DeleteMessage(ctx context.Context, chatID int64, msgID int) error {
	_, err := s.call(ctx, "deleteMessage", map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
	})
	return err
}

// SendDocument sends a file to the chat.
func (s *Sender) SendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string, threadID ...int) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	_ = w.WriteField("chat_id", fmt.Sprintf("%d", chatID))
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	if len(threadID) > 0 && threadID[0] > 0 {
		_ = w.WriteField("message_thread_id", fmt.Sprintf("%d", threadID[0]))
	}
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	_ = w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL("sendDocument"), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read sendDocument response: %w", err)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("unmarshal sendDocument response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram sendDocument error: %s", result.Description)
	}
	return nil
}

func buildKeyboard(rows [][]InlineButton) map[string]interface{} {
	tgRows := make([][]map[string]interface{}, len(rows))
	for i, row := range rows {
		tgRows[i] = make([]map[string]interface{}, len(row))
		for j, btn := range row {
			tgRows[i][j] = map[string]interface{}{
				"text":          btn.Text,
				"callback_data": btn.CallbackData,
			}
		}
	}
	return map[string]interface{}{
		"inline_keyboard": tgRows,
	}
}

// SetMyCommands sets the Telegram command menu for a given scope.
func (s *Sender) SetMyCommands(ctx context.Context, commands []BotCommand, scope map[string]interface{}) error {
	payload := map[string]interface{}{
		"commands": commands,
	}
	if scope != nil {
		payload["scope"] = scope
	}
	_, err := s.call(ctx, "setMyCommands", payload)
	return err
}

// RegisterCommandScopes registers public commands for everyone and includes /reset only for the owner's private chat scope.
func (s *Sender) RegisterCommandScopes(ctx context.Context, ownerID int64) error {
	publicCmds := []BotCommand{
		{Command: "journal", Description: "Catat trade (guided flow)"},
		{Command: "open", Description: "Lihat & tutup trade yang masih OPEN"},
		{Command: "history", Description: "Riwayat trade (Edit & Hapus)"},
		{Command: "undo", Description: "Hapus 1 trade terakhir"},
		{Command: "stats", Description: "Statistik trading (all/week/month)"},
		{Command: "leaderboard", Description: "Top 10 trader komunitas"},
		{Command: "profile", Description: "Profil level, XP & streak"},
		{Command: "badges", Description: "Koleksi badge pencapaian"},
		{Command: "challenge", Description: "Weekly challenge & klasemen"},
		{Command: "goal", Description: "Target bulanan & progress"},
		{Command: "reminder", Description: "Atur pengingat jurnal harian"},
		{Command: "analyze", Description: "Analisis trading dengan AI"},
		{Command: "reportcard", Description: "Rapor bulanan trader"},
		{Command: "export", Description: "Export jurnal ke CSV/PDF"},
		{Command: "report", Description: "Ringkasan mingguan komunitas"},
		{Command: "help", Description: "Panduan penggunaan bot"},
	}
	if err := s.SetMyCommands(ctx, publicCmds, map[string]interface{}{"type": "default"}); err != nil {
		return err
	}
	if ownerID > 0 {
		ownerCmds := append([]BotCommand{}, publicCmds...)
		ownerCmds = append(ownerCmds, BotCommand{
			Command:     "reset",
			Description: "Reset semua data BadgerDB & Notion (Owner Only)",
		})
		_ = s.SetMyCommands(ctx, ownerCmds, map[string]interface{}{
			"type":    "chat",
			"chat_id": ownerID,
		})
	}
	return nil
}
