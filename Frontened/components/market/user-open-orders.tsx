"use client";

import { type Dispatch, type SetStateAction, useEffect, useMemo, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { OpenOrderItem, OpenOrdersResponse, UserOrderSocketMessage } from "@/types/market";
import { usePrivy } from "@privy-io/react-auth";
import { toast } from "sonner";

export const UserOpenOrders = ({ marketId }: { marketId: string }) => {
  const { user, getAccessToken } = usePrivy();
  const [orders, setOrders] = useState<OpenOrderItem[]>([]);
  const [matchingEnabled, setMatchingEnabled] = useState(false);
  const [loading, setLoading] = useState(false);

  const hasOrders = useMemo(() => orders.length > 0, [orders]);

  useEffect(() => {
    let active = true;
    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    const load = async () => {
      if (!user) return;
      try {
        setLoading(true);
        const token = await getAccessToken();
        if (!token) return;
        const { data } = await api.get<OpenOrdersResponse>(`/orders/open/${marketId}`, {
          headers: { "privy-id-token": token },
        });
        if (!active) return;
        setOrders(data.orders || []);
        setMatchingEnabled(Boolean(data.matching_enabled));
      } catch (error) {
        console.error("load open orders failed", error);
      } finally {
        if (active) setLoading(false);
      }
    };

    void load();
    const connect = async () => {
      if (!active || !user) return;
      const token = await getAccessToken();
      if (!token) return;
      ws = new WebSocket(buildUserWSURL());
      ws.onopen = () => {
        ws?.send(JSON.stringify({ privy_token: token }));
      };
      ws.onmessage = (event) => {
        try {
          const payload = JSON.parse(event.data) as Partial<UserOrderSocketMessage>;
          if (!payload.order || payload.market_id !== marketId) return;
          applyOrderPatch(payload as UserOrderSocketMessage, setOrders);
        } catch (error) {
          console.error("user orders websocket parse failed", error);
        }
      };
      ws.onerror = () => undefined;
      ws.onclose = () => {
        if (!active) return;
        reconnectTimer = setTimeout(() => {
          void connect();
        }, 1200);
      };
    };
    void connect();
    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      ws?.close();
    };
  }, [marketId, user, getAccessToken]);

  const handleCancel = async (orderID: string) => {
    const token = await getAccessToken();
    if (!token) {
      toast.error("Missing auth token");
      return;
    }
    try {
      await api.delete(`/orders/${orderID}`, {
        params: { market_id: marketId },
        headers: { "privy-id-token": token },
      });
      setOrders((items) => items.filter((item) => item.id !== orderID));
      toast.success("Cancel command accepted");
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
      <table className="w-full text-left text-sm">
        <thead className="bg-zinc-50 text-zinc-500 dark:bg-zinc-900/60 dark:text-zinc-400">
          <tr>
            <th className="px-3 py-2 font-medium">Order ID</th>
            <th className="px-3 py-2 font-medium">Side</th>
            <th className="px-3 py-2 font-medium">Outcome</th>
            <th className="px-3 py-2 font-medium text-right">Action</th>
          </tr>
        </thead>
        <tbody>
          {orders.map((order) => (
            <tr key={order.id} className="border-t border-zinc-200 dark:border-zinc-800">
              <td className="max-w-[280px] truncate px-3 py-2 font-mono text-xs">{order.id}</td>
              <td className="px-3 py-2">{(order.side || "-").toString()}</td>
              <td className="px-3 py-2">{(order.outcome || "-").toString()}</td>
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

function buildUserWSURL(): string {
  const apiBase = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080/api";
  const parsed = new URL(apiBase, window.location.origin);
  const wsProtocol = parsed.protocol === "https:" ? "wss:" : "ws:";
  return `${wsProtocol}//${parsed.host}/ws/users/me`;
}

function applyOrderPatch(
  payload: UserOrderSocketMessage,
  setOrders: Dispatch<SetStateAction<OpenOrderItem[]>>,
) {
  const terminalStatuses = new Set([3, 4, 5, 6]);
  setOrders((items) => {
    if (terminalStatuses.has(payload.order.status)) {
      return items.filter((item) => item.id !== payload.order.id);
    }
    const nextItem: OpenOrderItem = {
      id: payload.order.id,
      side: payload.order.side,
      outcome: payload.order.outcome,
      price: payload.order.price,
      quantity: payload.order.quantity,
    };
    const existing = items.find((item) => item.id === payload.order.id);
    if (!existing) {
      return [nextItem, ...items];
    }
    return items.map((item) =>
      item.id === payload.order.id
        ? {
            ...item,
            ...Object.fromEntries(Object.entries(nextItem).filter(([, value]) => value !== undefined && value !== "")),
          }
        : item,
    );
  });
}
