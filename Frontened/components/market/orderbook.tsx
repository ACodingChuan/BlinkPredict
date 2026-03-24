"use client";

import { useEffect, useMemo, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { MarketDepthSocketMessage, OrderbookSnapshot } from "@/types/market";

interface OrderbookProps {
  outcome: "yes" | "no";
  marketId: string;
}

type BookRow = {
  side: "bid" | "ask";
  price: number;
  quantity: number;
};

const EMPTY_BOOK: OrderbookSnapshot = {
  bids: [],
  asks: [],
  matching_enabled: false,
};

export const Orderbook = ({ outcome, marketId }: OrderbookProps) => {
  const [snapshot, setSnapshot] = useState<OrderbookSnapshot>(EMPTY_BOOK);
  const [loading, setLoading] = useState(true);
  const [socketState, setSocketState] = useState<"connecting" | "live" | "offline">("connecting");

  useEffect(() => {
    let active = true;
    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

    const fetchOrderbook = async () => {
      try {
        const { data } = await api.get<OrderbookSnapshot>(`/orderbook/${marketId}`);
        if (!active) return;
        setSnapshot(data);
      } catch (error) {
        console.error("Failed to fetch orderbook", error);
      } finally {
        if (active) setLoading(false);
      }
    };

    const connectSocket = () => {
      if (!active) return;

      const wsURL = buildOrderbookWSURL(marketId);
      setSocketState("connecting");
      ws = new WebSocket(wsURL);

      ws.onopen = () => {
        if (!active) return;
        setSocketState("live");
      };

      ws.onmessage = (event) => {
        if (!active) return;
        try {
          const payload = JSON.parse(event.data) as Partial<MarketDepthSocketMessage>;
          if (payload.market_id !== marketId || !Array.isArray(payload.levels)) return;
          setSnapshot((current) => applyDepthLevels(current, payload as MarketDepthSocketMessage));
          setLoading(false);
        } catch (error) {
          console.error("Failed to parse websocket payload", error);
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
          connectSocket();
        }, 1200);
      };
    };

    void fetchOrderbook();
    connectSocket();

    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
        ws.close();
      }
    };
  }, [marketId]);

  const rows = useMemo(() => buildRows(snapshot, outcome), [snapshot, outcome]);
  const maxQty = useMemo(() => {
    if (rows.length === 0) return 1;
    return Math.max(...rows.map((row) => row.quantity), 1);
  }, [rows]);

  const askRows = rows.filter((row) => row.side === "ask");
  const bidRows = rows.filter((row) => row.side === "bid");
  const bestAsk = askRows.length > 0 ? askRows[0].price : 0;
  const bestBid = bidRows.length > 0 ? bidRows[0].price : 0;
  const latest = bestAsk > 0 ? bestAsk : bestBid;
  const spread = bestAsk > 0 && bestBid > 0 ? Math.max(0, bestAsk - bestBid) : 0;

  return (
    <section className="min-h-[520px] rounded-[28px] border border-zinc-200 bg-white shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <header className="flex items-center justify-between border-b border-zinc-200 px-6 py-4 dark:border-zinc-800">
        <div>
          <h3 className="text-2xl font-semibold text-zinc-900 dark:text-zinc-100">Orderbook</h3>
          <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">
            {outcome.toUpperCase()} market depth · {socketState === "live" ? "live" : socketState}
          </p>
        </div>
        <span
          className={`rounded-full px-3 py-1 text-xs font-semibold ${
            snapshot.matching_enabled
              ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/50 dark:text-emerald-300"
              : "bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200"
          }`}
        >
          matching {snapshot.matching_enabled ? "enabled" : "disabled"}
        </span>
      </header>

      <div className="px-6 py-5">
        <div className="mb-3 grid grid-cols-[2fr_1fr_1fr_1fr] text-sm font-medium text-zinc-500 dark:text-zinc-400">
          <div>Side</div>
          <div>Price</div>
          <div>Shares</div>
          <div className="text-right">Total</div>
        </div>

        {loading && rows.length === 0 ? (
          <div className="rounded-2xl border border-dashed border-zinc-300 px-4 py-10 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
            Loading orderbook...
          </div>
        ) : rows.length === 0 ? (
          <div className="rounded-2xl border border-dashed border-zinc-300 px-4 py-10 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
            No depth yet
          </div>
        ) : (
          <div className="overflow-hidden rounded-2xl border border-zinc-200 dark:border-zinc-800">
            {askRows.map((row) => (
              <Row key={`ask-${row.price}`} row={row} maxQty={maxQty} />
            ))}
            <div className="grid grid-cols-2 border-y border-zinc-200 bg-zinc-50 px-4 py-3 text-sm text-zinc-600 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-300">
              <div>Last: {formatCents(latest)}</div>
              <div className="text-right">Spread: {formatCents(spread)}</div>
            </div>
            {bidRows.map((row) => (
              <Row key={`bid-${row.price}`} row={row} maxQty={maxQty} />
            ))}
          </div>
        )}
      </div>
    </section>
  );
};

const Row = ({ row, maxQty }: { row: BookRow; maxQty: number }) => {
  const depth = Math.min(100, Math.max(0, (row.quantity / maxQty) * 100));
  const sideLabel = row.side === "ask" ? "ASK" : "BID";
  const priceClass = row.side === "ask" ? "text-rose-600 dark:text-rose-400" : "text-emerald-600 dark:text-emerald-400";
  const bgClass = row.side === "ask" ? "bg-rose-500/12" : "bg-emerald-500/12";
  const total = (row.price / 100) * row.quantity;

  return (
    <div className="relative grid grid-cols-[2fr_1fr_1fr_1fr] items-center border-t border-zinc-100 px-4 py-3 text-base dark:border-zinc-800">
      <div className={`absolute left-0 top-0 h-full ${bgClass}`} style={{ width: `${depth}%` }} />
      <div className="relative z-10 flex items-center gap-2 font-medium text-zinc-700 dark:text-zinc-200">
        <span className={`rounded-full px-2 py-0.5 text-xs font-semibold ${row.side === "ask" ? "bg-rose-100 text-rose-700 dark:bg-rose-900/40 dark:text-rose-300" : "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300"}`}>
          {sideLabel}
        </span>
      </div>
      <div className={`relative z-10 font-semibold ${priceClass}`}>{formatCents(row.price)}</div>
      <div className="relative z-10 text-zinc-800 dark:text-zinc-100">{formatQty(row.quantity)}</div>
      <div className="relative z-10 text-right font-medium text-zinc-800 dark:text-zinc-100">${total.toFixed(2)}</div>
    </div>
  );
};

function buildRows(snapshot: OrderbookSnapshot, outcome: "yes" | "no"): BookRow[] {
  const yesBids = snapshot.bids
    .map((row) => ({
      side: "bid" as const,
      price: toTick(row.price),
      quantity: toQty(row.total_volume),
    }))
    .filter((row) => row.price > 0 && row.quantity > 0);

  const yesAsks = snapshot.asks
    .map((row) => ({
      side: "ask" as const,
      price: toTick(row.price),
      quantity: toQty(row.total_volume),
    }))
    .filter((row) => row.price > 0 && row.quantity > 0);

  if (outcome === "yes") {
    const asks = yesAsks.sort((a, b) => b.price - a.price);
    const bids = yesBids.sort((a, b) => b.price - a.price);
    return [...asks, ...bids];
  }

  const noBids = yesAsks
    .map((row) => ({
      side: "bid" as const,
      price: 100 - row.price,
      quantity: row.quantity,
    }))
    .filter((row) => row.price > 0 && row.price < 100)
    .sort((a, b) => b.price - a.price);

  const noAsks = yesBids
    .map((row) => ({
      side: "ask" as const,
      price: 100 - row.price,
      quantity: row.quantity,
    }))
    .filter((row) => row.price > 0 && row.price < 100)
    .sort((a, b) => b.price - a.price);

  return [...noAsks, ...noBids];
}

function toTick(value: string): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return 0;
  return Math.round(parsed);
}

function toQty(value: string): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return 0;
  return parsed;
}

function formatCents(value: number): string {
  return `${Math.round(value)}¢`;
}

function formatQty(value: number): string {
  return value.toLocaleString(undefined, { maximumFractionDigits: 2 });
}

function buildOrderbookWSURL(marketId: string): string {
  const apiBase = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080/api";
  const parsed = new URL(apiBase, window.location.origin);
  const wsProtocol = parsed.protocol === "https:" ? "wss:" : "ws:";
  return `${wsProtocol}//${parsed.host}/ws/markets/${marketId}`;
}

function applyDepthLevels(snapshot: OrderbookSnapshot, payload: MarketDepthSocketMessage): OrderbookSnapshot {
  const bids = new Map(snapshot.bids.map((row) => [row.price, row.total_volume]));
  const asks = new Map(snapshot.asks.map((row) => [row.price, row.total_volume]));

  for (const level of payload.levels) {
    const target = level.side === 0 ? bids : asks;
    const key = level.price_tick.toString();
    if (level.total_volume === 0) {
      target.delete(key);
    } else {
      target.set(key, level.total_volume.toString());
    }
  }

  const nextBids = Array.from(bids.entries())
    .map(([price, totalVolume]) => ({ price, total_volume: totalVolume }))
    .sort((a, b) => Number(b.price) - Number(a.price));
  const nextAsks = Array.from(asks.entries())
    .map(([price, totalVolume]) => ({ price, total_volume: totalVolume }))
    .sort((a, b) => Number(a.price) - Number(b.price));

  return {
    bids: nextBids,
    asks: nextAsks,
    best_bid_price: nextBids[0]?.price,
    best_ask_price: nextAsks[0]?.price,
    matching_enabled: true,
  };
}
