package model

import (
	"time"

	"github.com/google/uuid"
)

// ScheduledCommand represents a recurring command that the bot will execute
// automatically in a WhatsApp group at a given cron schedule.
type ScheduledCommand struct {
	ID        uuid.UUID
	Name      string
	Schedule  string
	Command   string
	GroupJID  string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}
