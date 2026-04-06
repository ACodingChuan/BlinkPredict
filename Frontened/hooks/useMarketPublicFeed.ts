"use client";

import { useEffect, useRef, useState } from "react";
import {
  MarketDeltaSocketMessage,
  MarketPublicSocketMessage,
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

export function useMarketPublicFeed(marketId?: string): MarketPublicFeedState {
  const [state, setState] = useState<MarketPublicFeedState>({
    orderbook: EMPTY_ORDERBOOK,
    trades: [],
    priceHistory: [],
    loading: true,
    socketState: "connecting",
  });
  const seqRef = useRef("0");

  useEffect(() => {
    if (!marketId) {
      setState({
        orderbook: EMPTY_ORDERBOOK,
        trades: [],
        priceHistory: [],
        loading: true,
        socketState: "offline",
      });
      seqRef.current = "0";
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
          const payload = JSON.parse(event.data) as Partial<MarketPublicSocketMessage>;
          if (payload.market_id !== marketId || typeof payload.type !== "string") return;

          if (payload.type === "market.snapshot") {
            const snapshot = payload as MarketSnapshotSocketMessage;
            seqRef.current = normalizeSeq(snapshot.seq);
            setState({
              orderbook: {
                bids: snapshot.payload.orderbook.bids || [],
                asks: snapshot.payload.orderbook.asks || [],
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
          const delta = payload as MarketDeltaSocketMessage;
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
        reconnectTimer = setTimeout(connect, 1200);
      };
    };

    seqRef.current = "0";
    setState({
      orderbook: EMPTY_ORDERBOOK,
      trades: [],
      priceHistory: [],
      loading: true,
      socketState: "connecting",
    });
    connect();

    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
        ws.close();
      }
    };
  }, [marketId]);

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

function applyMarketDelta(current: MarketPublicFeedState, payload: MarketDeltaSocketMessage): MarketPublicFeedState {
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
    const target = level.side === "ask" ? asks : bids;
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
