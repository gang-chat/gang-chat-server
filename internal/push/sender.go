package push

import (
	"context"
	"errors"
)

var ErrUnregisteredToken = errors.New("push token is no longer registered")

// Notification is provider-neutral. Chat code supplies semantic identifiers;
// each platform sender translates them into its own wire format.
type Notification struct {
	Token       string
	MessageID   string
	RoomID      string
	RoomName    string
	SenderName  string
	Body        string
	UnreadCount int
}

type Sender interface {
	Provider() string
	Send(context.Context, Notification) error
}
