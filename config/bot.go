package config

import (
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// BotConfig holds all WhatsApp bot specific configuration
type BotConfig struct {
	// WhatsApp identity
	BotNumber          string   // Bot phone number (e.g. 34936674253)
	MentionedBotNumber string   // Bot JID in @lid form used in group mentions
	ToNotification     string   // JID that receives restart notifications
	TrollNumbers       []string // JIDs that get the "troll" AI personality

	// Kiosk folders and retentions (in hours)
	NacionalFolder         string
	RetentionNacional      int
	InternacionalFolder    string
	RetentionInternacional int
	MagazineFolder         string
	RetentionMagazine      int

	// URLServer is the public base URL (host:port) used when building download links
	URLServer string

	// Storage
	DBDriver      string // Application database driver (sqlite or mysql)
	DBPath        string // Application database path (sqlite only)
	DBDSN         string // Application database DSN (overrides db-path; required for mysql)
	DBAutoCreate  bool   // Automatically create the MySQL database if it does not exist
	SessionDBPath string // whatsmeow session store

	// External APIs
	OpenAIAPIKey          string
	OpenAIModel           string
	WeatherAPIKey         string
	WeatherAPIURL         string
	WeatherForecastAPIURL string
	WeatherNoteWords      int
	QuoteAPIKey           string
	QuoteAPIURL           string
	StabilityAPIKey       string
	EsiosAPIKey           string

	// CommandTimeout is the max execution time (seconds) for a WhatsApp command
	CommandTimeout int

	// PairPhone, when set and no session exists, requests a pairing code for
	// this phone number instead of showing a QR code
	PairPhone string

	// ElectricityCacheDir is where the kwh-*.json/png cache files are stored
	ElectricityCacheDir string

	// PDFChannelJID is the WhatsApp group/channel JID that receives automatic
	// PDF downloads. Empty means the feature is disabled.
	PDFChannelJID string

	// PDFCategory is the kiosk category where downloaded PDFs are saved.
	PDFCategory string

	// SchedulerTimezone is the IANA timezone used for cron schedules.
	SchedulerTimezone string

	// SchedulerEnabled controls whether the scheduled command runner starts.
	SchedulerEnabled bool
}

func (b *BotConfig) validate() error {
	var errs []string

	if b.BotNumber == "" {
		errs = append(errs, "bot number cannot be empty")
	}
	if b.ToNotification == "" {
		errs = append(errs, "notification JID cannot be empty")
	}

	for _, folder := range []struct{ name, value string }{
		{"nacional", b.NacionalFolder},
		{"internacional", b.InternacionalFolder},
		{"magazine", b.MagazineFolder},
	} {
		if folder.value == "" {
			errs = append(errs, fmt.Sprintf("%s folder cannot be empty", folder.name))
		}
	}

	for _, ret := range []struct {
		name  string
		value int
	}{
		{"nacional", b.RetentionNacional},
		{"internacional", b.RetentionInternacional},
		{"magazine", b.RetentionMagazine},
	} {
		if ret.value <= 0 {
			errs = append(errs, fmt.Sprintf("%s retention must be positive (hours)", ret.name))
		}
	}

	if b.URLServer == "" {
		errs = append(errs, "url server cannot be empty")
	}
	if b.DBDriver != "sqlite" && b.DBDriver != "mysql" {
		errs = append(errs, "db driver must be sqlite or mysql")
	}
	if b.DBDriver == "mysql" && b.DBDSN == "" {
		errs = append(errs, "db dsn is required for mysql")
	}
	if b.DBDriver == "sqlite" && b.DBDSN == "" && b.DBPath == "" {
		errs = append(errs, "db path or db dsn must be provided for sqlite")
	}
	if b.SessionDBPath == "" {
		errs = append(errs, "session db path cannot be empty")
	}
	if b.CommandTimeout <= 0 {
		errs = append(errs, "command timeout must be positive")
	}
	if b.SchedulerTimezone == "" {
		errs = append(errs, "scheduler timezone cannot be empty")
	} else if _, err := time.LoadLocation(b.SchedulerTimezone); err != nil {
		errs = append(errs, fmt.Sprintf("invalid scheduler timezone %q", b.SchedulerTimezone))
	}

	if b.PDFChannelJID != "" {
		jid, err := types.ParseJID(b.PDFChannelJID)
		if err != nil || jid.User == "" || jid.Server == "" {
			errs = append(errs, fmt.Sprintf("pdf channel JID %q is invalid", b.PDFChannelJID))
		}
		if strings.TrimSpace(b.PDFCategory) == "" {
			errs = append(errs, "pdf category cannot be empty when pdf channel JID is set")
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("bot configuration invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// DatabaseDSN returns the effective database DSN.
// If DBDSN is set it is used directly; otherwise a sqlite DSN is built from DBPath.
func (b *BotConfig) DatabaseDSN() string {
	if b.DBDSN != "" {
		return b.DBDSN
	}
	return fmt.Sprintf("file:%s?_foreign_keys=on", b.DBPath)
}
