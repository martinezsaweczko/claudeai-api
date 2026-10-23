package config

import "fmt"

type ServerConfig struct {
	Address      string
	Port         int
	Timeout      int
	ReadTimeout  int
	WriteTimeout int
}

func (s *ServerConfig) validate() error {
	if s.Address == "" {
		return fmt.Errorf("server address cannot be empty")
	}
	if s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("server port must be between 1 and 65535")
	}
	if s.Timeout < 0 {
		return fmt.Errorf("server timeout cannot be negative")
	}
	if s.ReadTimeout < 0 {
		return fmt.Errorf("server read timeout cannot be negative")
	}
	if s.WriteTimeout < 0 {
		return fmt.Errorf("server write timeout cannot be negative")
	}
	return nil
}
