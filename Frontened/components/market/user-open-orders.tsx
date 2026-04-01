"use client";

import { type Dispatch, type SetStateAction, useCallback, useEffect, useMemo, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { OpenOrderItem, OpenOrdersResponse, UserOrderSocketMessage } from "@/types/market";
import { usePrivy } from "@/lib/auth-client";
import { toast } from "sonner";

export const UserOpenOrders = ({ marketId, refreshKey }: { marketId: string; refreshKey?: string }) => {
  const { user, getAccessToken } = usePrivy();
  const [orders, setOrders] = useState<OpenOrderItem[]>([]);
  const [matchingEnabled, setMatchingEnabled] = useState(false);
  const [loading, setLoading] = useState(false);
  const [lastUpdatedAt, setLastUpdatedAt] = useState<string>("");
  const [socketState, setSocketState] = useState<"connecting" | "live" | "offline">("connecting");

  const hasOrders = useMemo(() => orders.length > 0, [orders]);

  const loadOrders = useCallback(
    async (options?: { silent?: boolean }) => {
      if (!user) return;
      try {
        if (!options?.silent) {
          setLoading(true);
        }
        const token = await getAccessToken();
        if (!token) return;
        const { data } = await api.get<OpenOrdersResponse>(`/orders/open/${marketId}`, {
          headers: { Authorization: `Bearer ${token}` },
        });
        setOrders(data.orders || []);
        setMatchingEnabled(Boolean(data.matching_enabled));
        setLastUpdatedAt(new Date().toISOString());
      } catch (error) {
        console.error("load open orders failed", error);
      } finally {
        if (!options?.silent) {
          setLoading(false);
        }
      }
    },
    [getAccessToken, marketId, user],
  );

  useEffect(() => {
    let active = true;
    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let resyncTimer: ReturnType<typeof setInterval> | null = null;

    void loadOrders();

    const connect = async () => {
      if (!active || !user) return;
      try {
        setSocketState("connecting");
        const token = await getAccessToken();
        if (!token) {
          setSocketState("offline");
          return;
        }
        const { data } = await api.post<{ ticket: string }>("/ws-ticket", {}, {
          headers: { Authorization: `Bearer ${token}` },
        });
        ws = new WebSocket(buildUserWSURL(data.ticket));
        ws.onopen = () => {
          if (!active) return;
          setSocketState("live");
        };
        ws.onmessage = (event) => {
          try {
            const payload = JSON.parse(event.data) as Partial<UserOrderSocketMessage>;
            if (payload.type !== "user.order.updated" || payload.market_id !== marketId || !payload.payload?.order_id) return;
            applyOrderPatch(payload as UserOrderSocketMessage, setOrders);
            setLastUpdatedAt(new Date().toISOString());
          } catch (error) {
            console.error("user orders websocket parse failed", error);
          }
        };
        ws.onerror = () => {
          if (!active) return;
          setSocketState("offline");
        };
        ws.onclose = () => {
          if (!active) return;
          setSocketState("offline");
          reconnectTimer = setTimeout(() => {
            void connect();
          }, 1200);
        };
      } catch (error) {
        console.error("connect user websocket failed", error);
        setSocketState("offline");
        reconnectTimer = setTimeout(() => {
          void connect();
        }, 3000);
      }
    };

    void connect();
    resyncTimer = setInterval(() => {
      if (!active || document.visibilityState !== "visible") return;
      void loadOrders({ silent: true });
    }, 60000);

    const handleVisibilityChange = () => {
      if (!active || document.visibilityState !== "visible") return;
      void loadOrders({ silent: true });
    };

    window.addEventListener("focus", handleVisibilityChange);
    document.addEventListener("visibilitychange", handleVisibilityChange);

    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (resyncTimer) clearInterval(resyncTimer);
      ws?.close();
      window.removeEventListener("focus", handleVisibilityChange);
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, [getAccessToken, loadOrders, marketId, user]);

  useEffect(() => {
    if (!refreshKey || !user) return;
    void loadOrders({ silent: true });
  }, [loadOrders, refreshKey, user]);

  const handleCancel = async (orderID: string) => {
    const token = await getAccessToken();
    if (!token) {
      toast.error("Missing auth token");
      return;
    }
    try {
      await api.delete(`/orders/${orderID}`, {
        params: { market_id: marketId },
        headers: { Authorization: `Bearer ${token}` },
      });
      toast.success("Cancel command accepted");
      await loadOrders({ silent: true });
    } catch (error: unknown) {
      const response = typeof error === "object" && error !== null && "response" in error ? (error as { response?: { data?: { message?: string } } }).response : undefined;
      toast.error(response?.data?.message || "Cancel request failed");
    }
  };

  if (!user) {
    return (
      <div className="rounded-3xl border border-dashed border-zinc-300 px-4 py-8 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
        Connect your wallet to view open orders.
      </div>
    );
  }

  if (!hasOrders) {
    return (
      <div className="rounded-3xl border border-dashed border-zinc-300 px-4 py-8 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
        {loading ? "Loading..." : matchingEnabled ? "No open orders." : "No open orders yet (matcher still in rollout)."}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto rounded-2xl border border-zinc-200 dark:border-zinc-800">
      <div className="flex items-center justify-between border-b border-zinc-200 bg-zinc-50 px-3 py-2 text-xs text-zinc-500 dark:border-zinc-800 dark:bg-zinc-900/60 dark:text-zinc-400">
        <span>{socketState === "live" ? "Private websocket live" : `Private websocket ${socketState}`}</span>
        <span>{lastUpdatedAt ? `Updated ${formatTime(lastUpdatedAt)}` : "Waiting for first sync"}</span>
      </div>
      <table className="w-full text-left text-sm">
        <thead className="bg-zinc-50 text-zinc-500 dark:bg-zinc-900/60 dark:text-zinc-400">
          <tr>
            <th className="px-3 py-2 font-medium">Order ID</th>
            <th className="px-3 py-2 font-medium">Side</th>
            <th className="px-3 py-2 font-medium">Outcome</th>
            <th className="px-3 py-2 font-medium">Price</th>
            <th className="px-3 py-2 font-medium">Remaining Qty</th>
            <th className="px-3 py-2 font-medium">Status</th>
            <th className="px-3 py-2 font-medium text-right">Action</th>
          </tr>
        </thead>
        <tbody>
          {orders.map((order) => (
            <tr key={order.id} className="border-t border-zinc-200 dark:border-zinc-800">
              <td className="max-w-[280px] truncate px-3 py-2 font-mono text-xs">{order.id}</td>
              <td className="px-3 py-2">{(order.side || "-").toString()}</td>
              <td className="px-3 py-2">{(order.outcome || "-").toString()}</td>
              <td className="px-3 py-2">{formatPrice(order.price)}</td>
              <td className="px-3 py-2">{formatQuantity(order.quantity)}</td>
              <td className="px-3 py-2">
                <span className={`rounded-full px-2 py-1 text-xs font-medium ${statusClassName(order.status)}`}>
                  {formatStatus(order.status)}
                </span>
              </td>
              <td className="px-3 py-2 text-right">
                <button className="rounded-lg border border-zinc-300 px-2 py-1 text-xs font-medium hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800" onClick={() => handleCancel(order.id)}>
                  Cancel
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
};

function buildUserWSURL(ticket: string): string {
  const apiBase = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080/api";
  const parsed = new URL(apiBase, window.location.origin);
  const wsProtocol = parsed.protocol === "https:" ? "wss:" : "ws:";
  return `${wsProtocol}//${parsed.host}/ws/orders?ticket=${encodeURIComponent(ticket)}`;
}

function applyOrderPatch(
  payload: UserOrderSocketMessage,
  setOrders: Dispatch<SetStateAction<OpenOrderItem[]>>,
) {
  const terminalStatuses = new Set(["filled", "canceled", "expired", "rejected"]);
  setOrders((items) => {
    if (terminalStatuses.has(payload.payload.status)) {
      return items.filter((item) => item.id !== payload.payload.order_id);
    }
    const nextItem: OpenOrderItem = {
      id: payload.payload.order_id,
      side: payload.payload.original_action,
      outcome: payload.payload.original_outcome,
      price: payload.payload.original_price_tick,
      quantity: payload.payload.remaining_qty_lots,
      status: payload.payload.status,
    };
    const existing = items.find((item) => item.id === payload.payload.order_id);
    if (!existing) {
      return [nextItem, ...items];
    }
    return items.map((item) =>
      item.id === payload.payload.order_id
        ? {
            ...item,
            ...Object.fromEntries(Object.entries(nextItem).filter(([, value]) => value !== undefined && value !== "")),
          }
        : item,
    );
  });
}

function formatTime(value: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleTimeString();
}

function formatPrice(value?: string): string {
  const parsed = Number(value || "");
  if (!Number.isFinite(parsed) || parsed <= 0) return "-";
  return `${Math.round(parsed)}¢`;
}

function formatQuantity(value?: string): string {
  const parsed = Number(value || "");
  if (!Number.isFinite(parsed)) return "-";
  return (parsed / 100).toLocaleString(undefined, { minimumFractionDigits: 0, maximumFractionDigits: 2 });
}

function formatStatus(status?: string): string {
  if (!status) return "Unknown";
  return status.replaceAll("_", " ");
}

function statusClassName(status?: string): string {
  switch (status) {
    case "partially_filled":
      return "bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200";
    case "filled":
      return "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-200";
    case "canceled":
    case "expired":
    case "rejected":
      return "bg-zinc-200 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-200";
    default:
      return "bg-sky-100 text-sky-800 dark:bg-sky-900/40 dark:text-sky-200";
  }
}
