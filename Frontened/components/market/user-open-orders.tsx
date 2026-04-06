"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { OpenOrderItem, OpenOrdersResponse } from "@/types/market";
import { usePrivy } from "@/lib/auth-client";
import { toast } from "sonner";

export const UserOpenOrders = ({ marketId, refreshKey }: { marketId: string; refreshKey?: string }) => {
  const { user, getAccessToken } = usePrivy();
  const [orders, setOrders] = useState<OpenOrderItem[]>([]);
  const [matchingEnabled, setMatchingEnabled] = useState(false);
  const [loading, setLoading] = useState(false);
  const [lastUpdatedAt, setLastUpdatedAt] = useState<string>("");

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
    if (!user) return;
    void loadOrders();

    const pollTimer = setInterval(() => {
      if (document.visibilityState !== "visible") return;
      void loadOrders({ silent: true });
    }, 15000);

    const handleVisibilityChange = () => {
      if (document.visibilityState !== "visible") return;
      void loadOrders({ silent: true });
    };

    window.addEventListener("focus", handleVisibilityChange);
    document.addEventListener("visibilitychange", handleVisibilityChange);

    return () => {
      clearInterval(pollTimer);
      window.removeEventListener("focus", handleVisibilityChange);
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, [loadOrders, user]);

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
        <span>HTTP polling every 15s</span>
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
