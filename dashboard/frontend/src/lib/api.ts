import type {
  PilotStatus,
  PilotConfig,
  PilotLogEntry,
  PromptsStatus,
  ResetPromptsResult,
} from "./types";

declare global {
  interface Window {
    go: {
      main: {
        App: {
          GetPilotStatus(): Promise<PilotStatus>;
          InstallPilotHooks(): Promise<void>;
          UninstallPilotHooks(): Promise<void>;
          StartPilotWrapper(): Promise<void>;
          StopPilotWrapper(): Promise<void>;
          GetPilotConfig(): Promise<PilotConfig>;
          SavePilotConfig(cfg: PilotConfig): Promise<void>;
          GetPilotLogs(): Promise<PilotLogEntry[]>;
          GetPromptsStatus(): Promise<PromptsStatus>;
          ResetPrompts(): Promise<ResetPromptsResult>;
        };
      };
    };
  }
}

const app = () => window.go.main.App;

export async function getPilotStatus(): Promise<PilotStatus> {
  return app().GetPilotStatus();
}

export async function installPilotHooks(): Promise<void> {
  return app().InstallPilotHooks();
}

export async function uninstallPilotHooks(): Promise<void> {
  return app().UninstallPilotHooks();
}

export async function startPilotWrapper(): Promise<void> {
  return app().StartPilotWrapper();
}

export async function stopPilotWrapper(): Promise<void> {
  return app().StopPilotWrapper();
}

export async function getPilotConfig(): Promise<PilotConfig> {
  return app().GetPilotConfig();
}

export async function savePilotConfig(cfg: PilotConfig): Promise<void> {
  return app().SavePilotConfig(cfg);
}

export async function getPilotLogs(): Promise<PilotLogEntry[]> {
  return app().GetPilotLogs();
}

export async function getPromptsStatus(): Promise<PromptsStatus> {
  return app().GetPromptsStatus();
}

export async function resetPrompts(): Promise<ResetPromptsResult> {
  return app().ResetPrompts();
}
