import { useEffect, useRef, useState, useCallback } from "react";
import { getServerToken } from "./api";
import type {
  PilotAction,
  PilotPendingApproval,
  PilotApprovalResolved,
} from "./types";

export type SSEConnectionState = "connecting" | "connected" | "disconnected";

export interface PilotSSEState {
  connectionState: SSEConnectionState;
  actions: PilotAction[];
  pendingApprovals: Map<string, PilotPendingApproval>;
}

export function usePilotSSE(port: number, enabled: boolean) {
  const [connectionState, setConnectionState] =
    useState<SSEConnectionState>("disconnected");
  const [actions, setActions] = useState<PilotAction[]>([]);
  const [pendingApprovals, setPendingApprovals] = useState<
    Map<string, PilotPendingApproval>
  >(new Map());
  const esRef = useRef<EventSource | null>(null);
  const retryRef = useRef<number>(0);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const connect = useCallback(() => {
    if (!enabled || port === 0) return;

    setConnectionState("connecting");
    const es = new EventSource(`http://localhost:${port}/events`);
    esRef.current = es;

    es.addEventListener("connected", () => {
      setConnectionState("connected");
      retryRef.current = 0;
      // Clear stale pending approvals — they live in server memory,
      // so after a reconnect (or server restart) they're gone.
      setPendingApprovals(new Map());
    });

    es.addEventListener("action", (e) => {
      const action: PilotAction = JSON.parse(e.data);
      setActions((prev) => [action, ...prev].slice(0, 200));
    });

    es.addEventListener("pending_approval", (e) => {
      const pending: PilotPendingApproval = JSON.parse(e.data);
      setPendingApprovals((prev) => {
        const next = new Map(prev);
        next.set(pending.id, pending);
        return next;
      });
    });

    es.addEventListener("approval_resolved", (e) => {
      const resolved: PilotApprovalResolved = JSON.parse(e.data);
      setPendingApprovals((prev) => {
        const next = new Map(prev);
        next.delete(resolved.id);
        return next;
      });
    });

    es.addEventListener("state_change", () => {
      // State changes are handled by the regular polling
    });

    es.onerror = () => {
      es.close();
      esRef.current = null;
      setConnectionState("disconnected");

      const delay = Math.min(1000 * Math.pow(2, retryRef.current), 10000);
      retryRef.current++;
      retryTimerRef.current = setTimeout(connect, delay);
    };
  }, [port, enabled]);

  useEffect(() => {
    connect();
    return () => {
      if (esRef.current) {
        esRef.current.close();
        esRef.current = null;
      }
      if (retryTimerRef.current) {
        clearTimeout(retryTimerRef.current);
      }
    };
  }, [connect]);

  return { connectionState, actions, pendingApprovals };
}

// pilot serve rejects every POST without this bearer when PILOT_SERVER_TOKEN
// is configured; the SSE stream and status reads stay public.
async function authHeaders(): Promise<HeadersInit> {
  const token = await getServerToken().catch(() => "");
  return token ? { Authorization: `Bearer ${token}` } : {};
}

async function postToServe(port: number, path: string): Promise<void> {
  const res = await fetch(`http://localhost:${port}${path}`, {
    method: "POST",
    headers: await authHeaders(),
  });
  if (!res.ok) {
    throw new Error(`pilot serve ${path}: HTTP ${res.status}`);
  }
}

export async function approvePending(
  port: number,
  id: string
): Promise<void> {
  await postToServe(port, `/approve/${id}`);
}

export async function rejectPending(
  port: number,
  id: string
): Promise<void> {
  await postToServe(port, `/reject/${id}`);
}
