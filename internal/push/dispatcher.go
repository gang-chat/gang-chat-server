package push

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

type RoomMessage struct {
	MessageID  string
	RoomID     string
	SenderID   string
	SenderName string
	Type       string
	Body       string
}

type Dispatcher struct {
	db      *sql.DB
	store   *Store
	senders map[string]Sender
	queue   chan RoomMessage
}

type deliveryTarget struct {
	Provider    string
	Token       string
	RoomName    string
	RecipientID string
	UnreadCount int
}

func NewDispatcher(db *sql.DB, senders ...Sender) *Dispatcher {
	byProvider := make(map[string]Sender, len(senders))
	for _, sender := range senders {
		if sender != nil {
			byProvider[sender.Provider()] = sender
		}
	}
	return &Dispatcher{
		db:      db,
		store:   &Store{DB: db},
		senders: byProvider,
		queue:   make(chan RoomMessage, 256),
	}
}

func (d *Dispatcher) Enabled() bool {
	return d != nil && len(d.senders) > 0
}

func (d *Dispatcher) Enqueue(message RoomMessage) {
	if !d.Enabled() || message.RoomID == "" || message.MessageID == "" {
		return
	}
	select {
	case d.queue <- message:
	default:
		log.Printf("push: queue full; dropped message notification %s", message.MessageID)
	}
}

func (d *Dispatcher) Run(ctx context.Context) {
	if !d.Enabled() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-d.queue:
			d.deliver(ctx, message)
		}
	}
}

func (d *Dispatcher) deliver(parent context.Context, message RoomMessage) {
	targets, err := d.targets(message.RoomID, message.SenderID)
	if err != nil {
		log.Printf("push: list targets for %s: %v", message.MessageID, err)
		return
	}
	body := messagePreview(message.Type, message.Body)
	for _, target := range targets {
		sender := d.senders[target.Provider]
		if sender == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		err := sender.Send(ctx, Notification{
			Token:       target.Token,
			MessageID:   message.MessageID,
			RoomID:      message.RoomID,
			RoomName:    target.RoomName,
			SenderName:  message.SenderName,
			Body:        body,
			UnreadCount: target.UnreadCount,
		})
		cancel()
		if errors.Is(err, ErrUnregisteredToken) {
			d.store.DeleteToken(target.Provider, target.Token)
			continue
		}
		if err != nil {
			log.Printf("push: %s delivery for %s failed: %v", target.Provider, message.MessageID, err)
		}
	}
}

func (d *Dispatcher) targets(roomID, senderID string) ([]deliveryTarget, error) {
	rows, err := d.db.Query(
		`SELECT pd.provider, pd.token,
		        COALESCE(NULLIF(rm.remark_name, ''), r.name),
		        rm.user_id
		 FROM room_memberships rm
		 JOIN rooms r ON r.id = rm.room_id
		 JOIN push_devices pd ON pd.user_id = rm.user_id
		 JOIN user_sessions us ON us.id = pd.session_id
		 JOIN users u ON u.id = pd.user_id
		 WHERE rm.room_id = ?
		   AND rm.user_id <> ?
		   AND rm.notification_level = 'all'
		   AND pd.notifications_enabled = 1
		   AND pd.platform = 'android'
		   AND us.revoked_at IS NULL
		   AND us.expires_at > UNIX_TIMESTAMP()
		   AND u.status = 'active'`,
		roomID,
		senderID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]deliveryTarget, 0)
	unreadByUser := make(map[string]int)
	for rows.Next() {
		var target deliveryTarget
		if err := rows.Scan(
			&target.Provider,
			&target.Token,
			&target.RoomName,
			&target.RecipientID,
		); err != nil {
			return nil, err
		}
		unread, ok := unreadByUser[target.RecipientID]
		if !ok {
			unread = d.totalUnreadCount(target.RecipientID)
			unreadByUser[target.RecipientID] = unread
		}
		target.UnreadCount = unread
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (d *Dispatcher) totalUnreadCount(userID string) int {
	var count int
	// This mirrors room unread semantics sufficiently for the launcher badge:
	// blocked rooms are excluded and messages at/before the stored read cursor
	// are not counted. A missing cursor means every visible message is unread.
	_ = d.db.QueryRow(
		`SELECT COUNT(*)
		 FROM messages m
		 JOIN room_memberships rm
		   ON rm.room_id = m.room_id AND rm.user_id = ?
		 LEFT JOIN room_reads rr
		   ON rr.room_id = m.room_id AND rr.user_id = ?
		 LEFT JOIN messages read_message
		   ON read_message.id = rr.last_read_message_id
		 WHERE rm.notification_level <> 'blocked'
		   AND m.sender_user_id <> ?
		   AND m.is_recalled = 0
		   AND m.is_force_deleted = 0
		   AND NOT (
		     m.type = 'system'
		     AND EXISTS (
		       SELECT 1
		       FROM JSON_TABLE(m.attachments_json, '$[*]' COLUMNS (
		         attachment_type VARCHAR(64) PATH '$.type' NULL ON EMPTY,
		         attachment_event VARCHAR(64) PATH '$.event' NULL ON EMPTY
		       )) attachment
		       WHERE LOWER(COALESCE(attachment.attachment_type, '')) = 'system'
		         AND LOWER(COALESCE(attachment.attachment_event, ''))
		           IN ('live_joined', 'live_left')
		     )
		   )
		   AND (
		     read_message.id IS NULL OR
		     m.created_at > read_message.created_at OR
		     (m.created_at = read_message.created_at AND m.id > read_message.id)
		   )`,
		userID,
		userID,
		userID,
	).Scan(&count)
	if count < 1 {
		return 1
	}
	return count
}

func messagePreview(messageType, body string) string {
	body = strings.TrimSpace(body)
	if messageType == "text" && body != "" {
		const maxRunes = 180
		if utf8.RuneCountInString(body) <= maxRunes {
			return body
		}
		runes := []rune(body)
		return string(runes[:maxRunes-1]) + "…"
	}
	switch messageType {
	case "sticker":
		return "[表情]"
	case "audio":
		return "[语音]"
	case "file":
		return "[文件]"
	default:
		return "收到一条新消息"
	}
}
