"use client";

import { useEffect, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { MarketTradeSocketMessage, TradeItem, TradesResponse } from "@/types/market";

export const RecentTrades = ({ marketId }: { marketId: string }) => {
  const [trades, setTrades] = useState<TradesResponse["trades"]>([]);
  const [matchingEnabled, setMatchingEnabled] = useState(false);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    let active = true;
    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    const load = async () => {
      try {
        setLoading(true);
        const { data } = await api.get<TradesResponse>(`/trades/${marketId}`);
        if (!active) return;
        setTrades(data.trades || []);
        setMatchingEnabled(Boolean(data.matching_enabled));
      } catch (error) {
        console.error("load trades failed", error);
      } finally {
        if (active) setLoading(false);
      }
    };

    const connect = () => {
      if (!active) return;
      const wsURL = buildMarketWSURL(marketId);
      ws = new WebSocket(wsURL);
      ws.onmessage = (event) => {
        try {
          const payload = JSON.parse(event.data) as Partial<MarketTradeSocketMessage>;
          if (payload.market_id !== marketId || !payload.trade_id) return;
          const trade = toTradeItem(payload as MarketTradeSocketMessage);
          setTrades((current) => {
            const next = [trade, ...current.filter((item) => item.id !== trade.id)];
            return next.slice(0, 100);
          });
        } catch (error) {
          console.error("trade websocket parse failed", error);
        }
      };
      ws.onerror = () => undefined;
      ws.onclose = () => {
        if (!active) return;
        reconnectTimer = setTimeout(connect, 1200);
      };
    };

    void load();
    connect();
    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      ws?.close();
    };
  }, [marketId]);

  if (trades.length === 0) {
    return (
      <div className="rounded-2xl border border-dashed border-zinc-300 px-4 py-8 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
        {loading ? "Loading..." : matchingEnabled ? "No trades yet." : "No trades yet (matcher still in rollout)."}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto rounded-2xl border border-zinc-200 dark:border-zinc-800">
      <table className="w-full text-left text-sm">
        <thead className="bg-zinc-50 text-zinc-500 dark:bg-zinc-900/60 dark:text-zinc-400">
          <tr>
            <th className="px-4 py-3 font-medium">Price</th>
            <th className="px-4 py-3 font-medium">Qty</th>
            <th className="px-4 py-3 font-medium">Time</th>
            <th className="px-4 py-3 font-medium">Trade ID</th>
          </tr>
        </thead>
        <tbody>
          {trades.map((trade) => (
            <tr key={trade.id} className="border-t border-zinc-200 dark:border-zinc-800">
              <td className="px-4 py-3 font-semibold text-zinc-800 dark:text-zinc-100">{formatPrice(trade.price)}</td>
              <td className="px-4 py-3 text-zinc-700 dark:text-zinc-200">{formatQty(trade.quantity)}</td>
              <td className="px-4 py-3 text-zinc-500 dark:text-zinc-400">{formatTime(trade.executed_at)}</td>
              <td className="max-w-[220px] truncate px-4 py-3 font-mono text-xs text-zinc-700 dark:text-zinc-200">{trade.id}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
};

function formatPrice(value?: string): string {
  const parsed = Number(value || "0");
  if (!Number.isFinite(parsed)) return "--";
  return `${Math.round(parsed)}¢`;
}

function formatQty(value?: string): string {
  const parsed = Number(value || "0");
  if (!Number.isFinite(parsed)) return "--";
  return parsed.toLocaleString(undefined, { maximumFractionDigits: 2 });
}

function formatTime(value?: string): string {
  if (!value) return "--";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString();
}

function buildMarketWSURL(marketId: string): string {
  const apiBase = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080/api";
  const parsed = new URL(apiBase, window.location.origin);
  const wsProtocol = parsed.protocol === "https:" ? "wss:" : "ws:";
  return `${wsProtocol}//${parsed.host}/ws/markets/${marketId}`;
}

function toTradeItem(payload: MarketTradeSocketMessage): TradeItem {
  return {
    id: payload.trade_id,
    price: payload.price_tick,
    quantity: payload.match_qty,
    executed_at: payload.executed_at,
  };
}
