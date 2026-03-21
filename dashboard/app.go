package main

import (
	"context"

	"pilot-dashboard/internal/pilot"
)

// App struct — all exported methods become Wails bindings.
type App struct {
	ctx context.Context
}

func NewApp() *App {
	return &App{}
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	setDockIcon(icon)

	// Auto-start pilot SSE server if binary exists and hooks are installed
	status := pilot.GetPilotStatus()
	if status.Available && status.HooksInstalled {
		_ = pilot.StartServe()
	}
}

func (a *App) Shutdown(ctx context.Context) {
	_ = pilot.StopServe()
}

func (a *App) GetPilotStatus() pilot.PilotStatus {
	return pilot.GetPilotStatus()
}

func (a *App) InstallPilotHooks() error {
	if err := pilot.InstallHooks(); err != nil {
		return err
	}
	return pilot.StartServe()
}

func (a *App) UninstallPilotHooks() error {
	_ = pilot.StopServe()
	_ = pilot.KillLingering()
	return pilot.UninstallHooks()
}

func (a *App) StartPilotWrapper() error {
	return pilot.StartWrapper()
}

func (a *App) StopPilotWrapper() error {
	return pilot.StopWrapper()
}

func (a *App) GetPilotConfig() (pilot.PilotConfig, error) {
	return pilot.ReadPilotConfig()
}

func (a *App) SavePilotConfig(cfg pilot.PilotConfig) error {
	return pilot.WritePilotConfig(cfg)
}
