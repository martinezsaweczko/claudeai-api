package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePEM writes one PEM block to a file inside dir and returns its path.
func writePEM(t *testing.T, dir, name, blockType string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
	return path
}

// testRSAKey generates a throwaway RSA key.
func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	return key
}

// writeKeyPair writes a PKCS#1 private key and its PKIX public key.
func writeKeyPair(t *testing.T, dir string, key *rsa.PrivateKey) (publicPath, privatePath string) {
	t.Helper()
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	publicPath = writePEM(t, dir, "public.pem", "PUBLIC KEY", pubDER)
	privatePath = writePEM(t, dir, "private.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	return publicPath, privatePath
}

func TestJWTConfigValidate(t *testing.T) {
	dir := t.TempDir()
	publicPath, privatePath := writeKeyPair(t, dir, testRSAKey(t))
	missing := filepath.Join(dir, "missing.pem")

	tests := []struct {
		name    string
		cfg     JWTConfig
		wantErr string
	}{
		{"both files exist", JWTConfig{PublicKeyPath: publicPath, PrivateKeyPath: privatePath}, ""},
		{"empty public path", JWTConfig{PrivateKeyPath: privatePath}, "public key path cannot be empty"},
		{"empty private path", JWTConfig{PublicKeyPath: publicPath}, "private key path cannot be empty"},
		{"public file missing", JWTConfig{PublicKeyPath: missing, PrivateKeyPath: privatePath}, "public key file not found"},
		{"private file missing", JWTConfig{PublicKeyPath: publicPath, PrivateKeyPath: missing}, "private key file not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestJWTConfigLoadKeys(t *testing.T) {
	key := testRSAKey(t)

	t.Run("loads a PKCS#1 pair", func(t *testing.T) {
		publicPath, privatePath := writeKeyPair(t, t.TempDir(), key)
		cfg := JWTConfig{PublicKeyPath: publicPath, PrivateKeyPath: privatePath}

		if err := cfg.LoadKeys(); err != nil {
			t.Fatalf("LoadKeys failed: %v", err)
		}
		if cfg.PrivateKey == nil || !cfg.PrivateKey.Equal(key) {
			t.Error("loaded private key differs from the one written")
		}
		if cfg.PublicKey == nil || !cfg.PublicKey.Equal(&key.PublicKey) {
			t.Error("loaded public key differs from the one written")
		}
	})

	t.Run("loads a PKCS#8 private key", func(t *testing.T) {
		dir := t.TempDir()
		publicPath, _ := writeKeyPair(t, dir, key)
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("failed to marshal PKCS#8 key: %v", err)
		}
		cfg := JWTConfig{PublicKeyPath: publicPath, PrivateKeyPath: writePEM(t, dir, "pkcs8.pem", "PRIVATE KEY", der)}

		if err := cfg.LoadKeys(); err != nil {
			t.Fatalf("LoadKeys failed: %v", err)
		}
		if !cfg.PrivateKey.Equal(key) {
			t.Error("loaded PKCS#8 private key differs from the one written")
		}
	})

	t.Run("fails validation before reading", func(t *testing.T) {
		cfg := JWTConfig{}
		if err := cfg.LoadKeys(); err == nil {
			t.Fatal("expected an error for empty paths")
		}
		if cfg.PrivateKey != nil || cfg.PublicKey != nil {
			t.Error("no key should be set when validation fails")
		}
	})

	t.Run("rejects an unparseable private key", func(t *testing.T) {
		dir := t.TempDir()
		publicPath, _ := writeKeyPair(t, dir, key)
		garbage := filepath.Join(dir, "garbage.pem")
		if err := os.WriteFile(garbage, []byte("not a pem file"), 0o600); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		cfg := JWTConfig{PublicKeyPath: publicPath, PrivateKeyPath: garbage}

		err := cfg.LoadKeys()
		if err == nil || !strings.Contains(err.Error(), "failed to parse private key") {
			t.Fatalf("error = %v, want a private key parse failure", err)
		}
	})

	t.Run("rejects an unparseable public key", func(t *testing.T) {
		dir := t.TempDir()
		_, privatePath := writeKeyPair(t, dir, key)
		garbage := filepath.Join(dir, "garbage.pem")
		if err := os.WriteFile(garbage, []byte("not a pem file"), 0o600); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		cfg := JWTConfig{PublicKeyPath: garbage, PrivateKeyPath: privatePath}

		err := cfg.LoadKeys()
		if err == nil || !strings.Contains(err.Error(), "failed to parse public key") {
			t.Fatalf("error = %v, want a public key parse failure", err)
		}
		if cfg.PublicKey != nil {
			t.Error("public key should stay unset on failure")
		}
	})
}

func TestParseKeysRejectNonRSA(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate EC key: %v", err)
	}
	ecPrivate, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("failed to marshal EC private key: %v", err)
	}
	ecPublic, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal EC public key: %v", err)
	}

	if _, err := parsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecPrivate})); err == nil ||
		!strings.Contains(err.Error(), "not an RSA key") {
		t.Errorf("EC private key: error = %v, want a non-RSA rejection", err)
	}
	if _, err := parsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecPublic})); err == nil ||
		!strings.Contains(err.Error(), "not an RSA public key") {
		t.Errorf("EC public key: error = %v, want a non-RSA rejection", err)
	}

	// A valid PEM envelope whose payload is not a key at all.
	junk := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})
	if _, err := parsePrivateKey(junk); err == nil {
		t.Error("junk private key payload should be rejected")
	}
	if _, err := parsePublicKey(junk); err == nil {
		t.Error("junk public key payload should be rejected")
	}
}

func TestLogConfigValidate(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		cfg     LogConfig
		wantErr string
	}{
		{"debug to stdout", LogConfig{Level: "debug", FilePath: "stdout"}, ""},
		{"info", LogConfig{Level: "info"}, ""},
		{"warn", LogConfig{Level: "warn"}, ""},
		{"error", LogConfig{Level: "error"}, ""},
		{"file in an existing directory", LogConfig{Level: "info", FilePath: filepath.Join(dir, "app.log")}, ""},
		{"blank file path is ignored", LogConfig{Level: "info", FilePath: "   "}, ""},
		{"unknown level", LogConfig{Level: "trace"}, "invalid log level: trace"},
		{"level is case sensitive", LogConfig{Level: "INFO"}, "invalid log level: INFO"},
		{"empty level", LogConfig{}, "invalid log level"},
		{"file in a missing directory", LogConfig{Level: "info", FilePath: filepath.Join(dir, "nope", "app.log")}, "log file directory does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run("level "+tt.level, func(t *testing.T) {
			if got := ParseLogLevel(tt.level); got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestLogFilePath(t *testing.T) {
	t.Run("stdout keyword returns os.Stdout", func(t *testing.T) {
		f, err := LogFilePath("stdout")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != os.Stdout {
			t.Error("expected os.Stdout")
		}
	})

	t.Run("creates the file and appends to it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		for _, line := range []string{"first\n", "second\n"} {
			f, err := LogFilePath(path)
			if err != nil {
				t.Fatalf("LogFilePath failed: %v", err)
			}
			if _, err := f.WriteString(line); err != nil {
				t.Fatalf("write failed: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close failed: %v", err)
			}
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		if string(got) != "first\nsecond\n" {
			t.Errorf("file content = %q, want both lines appended", got)
		}
	})

	t.Run("missing directory is an error", func(t *testing.T) {
		if _, err := LogFilePath(filepath.Join(t.TempDir(), "nope", "app.log")); err == nil {
			t.Fatal("expected an error for a missing directory")
		}
	})
}

func TestServerConfigValidate(t *testing.T) {
	valid := ServerConfig{Address: "0.0.0.0", Port: 46564, Timeout: 30, ReadTimeout: 10, WriteTimeout: 30}

	tests := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr string
	}{
		{"valid", func(*ServerConfig) {}, ""},
		{"zero timeouts are allowed", func(c *ServerConfig) { c.Timeout, c.ReadTimeout, c.WriteTimeout = 0, 0, 0 }, ""},
		{"lowest port", func(c *ServerConfig) { c.Port = 1 }, ""},
		{"highest port", func(c *ServerConfig) { c.Port = 65535 }, ""},
		{"empty address", func(c *ServerConfig) { c.Address = "" }, "address cannot be empty"},
		{"port zero", func(c *ServerConfig) { c.Port = 0 }, "port must be between 1 and 65535"},
		{"port above range", func(c *ServerConfig) { c.Port = 65536 }, "port must be between 1 and 65535"},
		{"negative timeout", func(c *ServerConfig) { c.Timeout = -1 }, "server timeout cannot be negative"},
		{"negative read timeout", func(c *ServerConfig) { c.ReadTimeout = -1 }, "read timeout cannot be negative"},
		{"negative write timeout", func(c *ServerConfig) { c.WriteTimeout = -1 }, "write timeout cannot be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			err := cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBotConfigDatabaseDSN(t *testing.T) {
	tests := []struct {
		name string
		cfg  BotConfig
		want string
	}{
		{"sqlite path becomes a file DSN", BotConfig{DBPath: "/data/db.sqlite"}, "file:/data/db.sqlite?_foreign_keys=on"},
		{"explicit DSN is used as is", BotConfig{DBDSN: "user:pass@tcp(db:3306)/bot?parseTime=true"}, "user:pass@tcp(db:3306)/bot?parseTime=true"},
		{"explicit DSN wins over the path", BotConfig{DBPath: "/data/db.sqlite", DBDSN: "file:/other.sqlite"}, "file:/other.sqlite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.DatabaseDSN(); got != tt.want {
				t.Errorf("DatabaseDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBotConfigValidateDatabaseAndPDFChannel(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*BotConfig)
		wantErr string
	}{
		{"mysql with a DSN", func(c *BotConfig) { c.DBDriver, c.DBDSN, c.DBPath = "mysql", "user:pass@tcp(db:3306)/bot", "" }, ""},
		{"sqlite with only a DSN", func(c *BotConfig) { c.DBPath, c.DBDSN = "", "file:/data/db.sqlite" }, ""},
		{"pdf channel with category", func(c *BotConfig) { c.PDFChannelJID, c.PDFCategory = "120363420531996267@g.us", "nacional" }, ""},
		{"pdf category is ignored without a channel", func(c *BotConfig) { c.PDFCategory = "" }, ""},
		{"mysql without a DSN", func(c *BotConfig) { c.DBDriver, c.DBDSN = "mysql", "" }, "db dsn is required for mysql"},
		{"pdf channel without a server", func(c *BotConfig) { c.PDFChannelJID, c.PDFCategory = "120363420531996267", "nacional" }, "pdf channel JID"},
		{"pdf channel without a category", func(c *BotConfig) { c.PDFChannelJID, c.PDFCategory = "120363420531996267@g.us", "  " }, "pdf category cannot be empty"},
		{"negative retention", func(c *BotConfig) { c.RetentionInternacional = -5 }, "internacional retention must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBotConfig()
			tt.mutate(&cfg)
			err := cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBotConfigValidateAggregatesErrors(t *testing.T) {
	cfg := validBotConfig()
	cfg.BotNumber = ""
	cfg.URLServer = ""
	cfg.CommandTimeout = 0

	err := cfg.validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	for _, want := range []string{"bot number cannot be empty", "url server cannot be empty", "command timeout must be positive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should report %q", err, want)
		}
	}
}

func TestConfigErrorWithoutAppendedErrors(t *testing.T) {
	err := &ConfigError{Message: "Invalid configuration"}
	if got := err.Error(); got != "Invalid configuration" {
		t.Errorf("Error() = %q, want the bare message", got)
	}
}
