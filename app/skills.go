package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http/handlers"
	"github.com/martinezsaweczko/claudeai-api/services/skills"
)

const (
	// initialSkillsSyncTimeout bounds the sync done before the servers start so
	// a slow or unreachable remote cannot delay startup for long.
	initialSkillsSyncTimeout = 20 * time.Second
	// skillsStopTimeout bounds how long shutdown waits for the sync loop.
	skillsStopTimeout = 5 * time.Second
)

// skillsSyncer is the syncer capability the app uses. It is satisfied by
// *skills.Syncer; tests substitute fakes.
type skillsSyncer interface {
	handlers.SkillsSource
	Sync(ctx context.Context) error
	Run(ctx context.Context)
}

var _ skillsSyncer = (*skills.Syncer)(nil)

// newSkillsSyncer builds the syncer when skills are configured. A
// misconfiguration (for example a missing key file) is a startup error, unlike
// transient git failures, which only warn.
func (app *App) newSkillsSyncer() error {
	if app.skills != nil || !app.config.Skills.Enabled() {
		return nil
	}
	opts := []skills.Option{skills.WithLogger(app.log)}
	if app.observability != nil && app.observability.Trace != nil {
		opts = append(opts, skills.WithTracerProvider(app.observability.Trace))
	}
	s, err := skills.New(app.config.Skills, opts...)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	app.skills = s
	return nil
}

// skillsProvider adapts the syncer to claude.WithSkills: the current snapshot
// directory, or ok=false when there is no snapshot or it holds no skills.
func skillsProvider(src interface {
	Current() (skills.Snapshot, bool)
}) func() (string, bool) {
	return func() (string, bool) {
		snap, ok := src.Current()
		if !ok || len(snap.Skills) == 0 || snap.Dir == "" {
			return "", false
		}
		return snap.Dir, true
	}
}

// logSkillsStartup logs the skills posture. The repository URL cannot carry
// credentials (config validation rejects them) and key contents are never read.
func logSkillsStartup(log *slog.Logger, c config.SkillsConfig) {
	if !c.Enabled() {
		log.Info("Skills disabled (set -skills-repo to enable)")
		return
	}
	log.Info("Skills enabled",
		"repo", c.Repo,
		"ref", c.Ref,
		"refresh", c.Refresh.String(),
		"state_dir", c.Dir,
		"ssh_key_configured", c.SSHKey != "",
		"known_hosts_configured", c.KnownHosts != "")
}

// startSkills runs one bounded initial sync, then the background sync loop.
// A failed or timed-out initial sync only warns: the loop keeps retrying with
// backoff and requests run without skills until a snapshot exists.
func (app *App) startSkills() {
	if app.skills == nil {
		return
	}
	ctx, cancel := context.WithTimeout(app.ctx, app.initialSyncTimeout())
	err := app.skills.Sync(ctx)
	cancel()
	if err != nil {
		app.log.Warn("Initial skills sync failed; continuing without a snapshot, retrying in the background", "error", err.Error())
	}

	runCtx, stop := context.WithCancel(app.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.skills.Run(runCtx)
	}()
	app.skillsStop = func() error {
		stop()
		select {
		case <-done:
			return nil
		case <-time.After(skillsStopTimeout):
			return errors.New("timed out waiting for the skills sync loop to stop")
		}
	}
}

func (app *App) initialSyncTimeout() time.Duration {
	if app.initialSkillsTimeout > 0 {
		return app.initialSkillsTimeout
	}
	return initialSkillsSyncTimeout
}

// stopSkills cancels the sync loop and waits for it (bounded).
func (app *App) stopSkills() error {
	if app.skillsStop == nil {
		return nil
	}
	stop := app.skillsStop
	app.skillsStop = nil
	return stop()
}

// closeBackend releases backend resources (the temp working directory).
func (app *App) closeBackend() error {
	if c, ok := app.claude.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
