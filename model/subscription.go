package model

import (
	"time"

	"github.com/google/uuid"
)

// Subscription represents a user's keyword subscription for file notifications
type Subscription struct {
	ID               uuid.UUID
	SubscriptionText string
	User             string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// FileUsage represents an access attempt to a file on the download server
type FileUsage struct {
	File   string
	Result int
}

// UserUsage represents a download link requested by a user
type UserUsage struct {
	User string
	File string
}
