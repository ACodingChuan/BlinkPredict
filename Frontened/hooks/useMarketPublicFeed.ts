"use client";

import { useEffect, useRef, useState } from "react";
import {
  MarketDeltaSocketMessage,
  MarketSnapshotSocketMessage,
  OrderbookSnapshot,
  PricePoint,
  TradeItem,
} from "@/types/market";

const EMPTY_ORDERBOOK: OrderbookSnapshot = {
  bids: [],
  asks: [],
  matching_enabled: false,
};

const PRICE_HISTORY_LIMIT = 4096;
const RECENT_TRADES_LIMIT = 100;

export type MarketPublicFeedState = {
  orderbook: OrderbookSnapshot;
  trades: TradeItem[];
  priceHistory: PricePoint[];
  loading: boolean;
  socketState: "connecting" | "live" | "offline";
};

type MarketPublicFeedCache = MarketPublicFeedState & {
  marketId?: string;
};

export function useMarketPublicFeed(marketId?: string): MarketPublicFeedState {
  const offlineState: MarketPublicFeedState = {
    orderbook: EMPTY_ORDERBOOK,
    trades: [],
    priceHistory: [],
    loading: true,
    socketState: "offline",
  };
  const [state, setState] = useState<MarketPublicFeedCache>({
    marketId: marketId || undefined,
    orderbook: EMPTY_ORDERBOOK,
    trades: [],
    priceHistory: [],
    loading: true,
    socketState: "connecting",
  });
  const seqRef = useRef("0");
  const hasSnapshotRef = useRef(false);
  const reconnectAttemptsRef = useRef(0);

  useEffect(() => {
    if (!marketId) {
      seqRef.current = "0";
      hasSnapshotRef.current = false;
      reconnectAttemptsRef.current = 0;
      return;
    }

    let active = true;
    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

    const connect = () => {
      if (!active) return;
      setState((current) => ({ ...current, socketState: "connecting" }));
      ws = new WebSocket(buildMarketWSURL(marketId));

      ws.onmessage = (event) => {
        if (!active) return;
        try {
          const payload = JSON.parse(event.data) as unknown;
          if (!isRecord(payload)) return;
          if (payload.market_id !== marketId || typeof payload.type !== "string") return;

          if (payload.type === "market.snapshot") {
            const snapshot = toSnapshotMessage(payload);
            if (!snapshot) return;
            hasSnapshotRef.current = true;
            reconnectAttemptsRef.current = 0;
            seqRef.current = normalizeSeq(snapshot.seq);
            setState({
              marketId,
              orderbook: {
                bids: snapshot.payload.orderbook.bids,
                asks: snapshot.payload.orderbook.asks,
                best_bid_price: snapshot.payload.orderbook.best_bid_price,
                best_ask_price: snapshot.payload.orderbook.best_ask_price,
                matching_enabled: Boolean(snapshot.payload.matching_enabled),
              },
              trades: (snapshot.payload.trades || []).map(toTradeItem),
              priceHistory: (snapshot.payload.price_history || []).map((point) => ({
                timestamp: point.timestamp,
                price: point.price,
                quantity: point.quantity,
              })),
              loading: false,
              socketState: "live",
            });
            return;
          }

          if (payload.type !== "market.delta") return;
          if (!hasSnapshotRef.current) return;
          const delta = toDeltaMessage(payload);
          if (!delta) return;
          if (compareSeq(delta.seq, seqRef.current) <= 0) return;
          seqRef.current = normalizeSeq(delta.seq);
          setState((current) => applyMarketDelta(current, delta));
        } catch (error) {
          console.error("market public websocket parse failed", error);
        }
      };

      ws.onerror = () => {
        if (!active) return;
        setState((current) => ({ ...current, socketState: "offline" }));
      };

      ws.onclose = () => {
        if (!active) return;
        setState((current) => ({ ...current, socketState: "offline" }));
        const attempts = reconnectAttemptsRef.current;
        const delayMs = Math.min(5000, 600 * 2 ** Math.min(attempts, 3));
        reconnectAttemptsRef.current = attempts + 1;
        if (reconnectTimer) clearTimeout(reconnectTimer);
        reconnectTimer = setTimeout(connect, delayMs);
      };
    };

    seqRef.current = "0";
    hasSnapshotRef.current = false;
    reconnectAttemptsRef.current = 0;
    connect();

    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
        ws.close();
      }
    };
  }, [marketId]);

  if (!marketId) {
    return offlineState;
  }
  if (state.marketId !== marketId) {
    return {
      orderbook: EMPTY_ORDERBOOK,
      trades: [],
      priceHistory: [],
      loading: true,
      socketState: state.socketState === "offline" ? "offline" : "connecting",
    };
  }
  return state;
}

function buildMarketWSURL(marketId: string): string {
  const apiBase = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080/api";
  const parsed = new URL(apiBase, window.location.origin);
  const wsProtocol = parsed.protocol === "https:" ? "wss:" : "ws:";
  return `${wsProtocol}//${parsed.host}/ws/markets/${marketId}`;
}

function normalizeSeq(value?: string): string {
  const normalized = (value || "").trim();
  if (!/^\d+$/.test(normalized)) return "0";
  return normalized.replace(/^0+(?=\d)/, "") || "0";
}

function compareSeq(left?: string, right?: string): number {
  const a = normalizeSeq(left);
  const b = normalizeSeq(right);
  if (a.length !== b.length) {
    return a.length > b.length ? 1 : -1;
  }
  if (a === b) return 0;
  return a > b ? 1 : -1;
}

function applyMarketDelta(current: MarketPublicFeedCache, payload: MarketDeltaSocketMessage): MarketPublicFeedCache {
  const nextOrderbook = applyDepthLevels(current.orderbook, payload);
  const nextTrades = [...(payload.payload.trades || []).map(toTradeItem), ...current.trades]
    .filter((trade, index, items) => items.findIndex((item) => item.id === trade.id) === index)
    .slice(0, RECENT_TRADES_LIMIT);
  const nextPriceHistory = [...current.priceHistory, ...((payload.payload.price_points || []).map((point) => ({
    timestamp: point.timestamp,
    price: point.price,
    quantity: point.quantity,
  })))]
    .slice(-PRICE_HISTORY_LIMIT);

  return {
    marketId: current.marketId,
    orderbook: nextOrderbook,
    trades: nextTrades,
    priceHistory: nextPriceHistory,
    loading: false,
    socketState: "live",
  };
}

function applyDepthLevels(snapshot: OrderbookSnapshot, payload: MarketDeltaSocketMessage): OrderbookSnapshot {
  const bids = new Map(snapshot.bids.map((row) => [row.price, row.total_volume]));
  const asks = new Map(snapshot.asks.map((row) => [row.price, row.total_volume]));

  for (const level of payload.payload.depth_levels || []) {
    const key = level.price_tick.toString();
    const target = level.side === "ask" ? asks : level.side === "bid" ? bids : null;
    if (!target) continue;
    if (!Number.isFinite(level.total_volume) || level.total_volume <= 0) {
      target.delete(key);
      continue;
    }
    target.set(key, level.total_volume.toString());
  }

  const nextBids = Array.from(bids.entries())
    .map(([price, total_volume]) => ({ price, total_volume }))
    .sort((a, b) => Number(b.price) - Number(a.price));
  const nextAsks = Array.from(asks.entries())
    .map(([price, total_volume]) => ({ price, total_volume }))
    .sort((a, b) => Number(a.price) - Number(b.price));

  return {
    bids: nextBids,
    asks: nextAsks,
    best_bid_price: nextBids[0]?.price,
    best_ask_price: nextAsks[0]?.price,
    matching_enabled: snapshot.matching_enabled,
  };
}

function toTradeItem(trade: { trade_id: string; price_tick: string; fill_amount: string; executed_at: string }): TradeItem {
  return {
    id: trade.trade_id,
    price: trade.price_tick,
    quantity: trade.fill_amount,
    executed_at: trade.executed_at,
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function toSnapshotMessage(value: Record<string, unknown>): MarketSnapshotSocketMessage | null {
  const payload = value.payload;
  if (!isRecord(payload)) return null;
  const orderbook = payload.orderbook;
  if (!isRecord(orderbook)) return null;

  return {
    type: "market.snapshot",
    market_id: String(value.market_id ?? ""),
    seq: String(value.seq ?? "0"),
    ts: String(value.ts ?? ""),
    payload: {
      matching_enabled: Boolean(payload.matching_enabled),
      orderbook: {
        bids: normalizeOrderbookLevels(orderbook.bids),
        asks: normalizeOrderbookLevels(orderbook.asks),
        best_bid_price: toOptionalString(orderbook.best_bid_price),
        best_ask_price: toOptionalString(orderbook.best_ask_price),
      },
      trades: normalizeTrades(payload.trades),
      price_history: normalizePricePoints(payload.price_history),
    },
  };
}

function toDeltaMessage(value: Record<string, unknown>): MarketDeltaSocketMessage | null {
  const payload = value.payload;
  if (!isRecord(payload)) return null;

  return {
    type: "market.delta",
    market_id: String(value.market_id ?? ""),
    seq: String(value.seq ?? "0"),
    ts: String(value.ts ?? ""),
    payload: {
      depth_levels: normalizeDepthLevels(payload.depth_levels),
      trades: normalizeTrades(payload.trades),
      price_points: normalizePricePoints(payload.price_points),
    },
  };
}

function normalizeOrderbookLevels(value: unknown): Array<{ price: string; total_volume: string }> {
  if (!Array.isArray(value)) return [];
  const rows: Array<{ price: string; total_volume: string }> = [];
  for (const item of value) {
    if (!isRecord(item)) continue;
    const price = toOptionalString(item.price);
    const totalVolume = toOptionalString(item.total_volume);
    if (!price || !totalVolume) continue;
    rows.push({ price, total_volume: totalVolume });
  }
  return rows;
}

function normalizeTrades(value: unknown): Array<{ trade_id: string; price_tick: string; fill_amount: string; executed_at: string }> {
  if (!Array.isArray(value)) return [];
  const trades: Array<{ trade_id: string; price_tick: string; fill_amount: string; executed_at: string }> = [];
  for (const item of value) {
    if (!isRecord(item)) continue;
    const tradeID = toOptionalString(item.trade_id);
    const priceTick = toOptionalString(item.price_tick);
    const fillAmount = toOptionalString(item.fill_amount);
    const executedAt = toOptionalString(item.executed_at);
    if (!tradeID || !priceTick || !fillAmount || !executedAt) continue;
    trades.push({
      trade_id: tradeID,
      price_tick: priceTick,
      fill_amount: fillAmount,
      executed_at: executedAt,
    });
  }
  return trades;
}

function normalizePricePoints(value: unknown): Array<{ timestamp: string; price: string; quantity?: string }> {
  if (!Array.isArray(value)) return [];
  const points: Array<{ timestamp: string; price: string; quantity?: string }> = [];
  for (const item of value) {
    if (!isRecord(item)) continue;
    const timestamp = toOptionalString(item.timestamp);
    const price = toOptionalString(item.price);
    if (!timestamp || !price) continue;
    const quantity = toOptionalString(item.quantity);
    points.push({ timestamp, price, quantity });
  }
  return points;
}

function normalizeDepthLevels(value: unknown): Array<{ side: "bid" | "ask"; price_tick: number; total_volume: number }> {
  if (!Array.isArray(value)) return [];
  const levels: Array<{ side: "bid" | "ask"; price_tick: number; total_volume: number }> = [];
  for (const item of value) {
    if (!isRecord(item)) continue;
    const side = toOptionalString(item.side);
    if (side !== "bid" && side !== "ask") continue;
    const priceTick = toNumber(item.price_tick);
    const totalVolume = toNumber(item.total_volume);
    if (priceTick === undefined || totalVolume === undefined) continue;
    levels.push({
      side,
      price_tick: priceTick,
      total_volume: totalVolume,
    });
  }
  return levels;
}

function toOptionalString(value: unknown): string | undefined {
  if (typeof value === "string") return value;
  if (typeof value === "number" && Number.isFinite(value)) return value.toString();
  return undefined;
}

function toNumber(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "") {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return undefined;
}
