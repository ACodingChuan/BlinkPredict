"use client";

import { useEffect, useMemo, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { Market, MarketTradeSocketMessage, PriceHistoryRange, PriceHistoryResponse } from "@/types/market";

const RANGES: PriceHistoryRange[] = ["1H", "6H", "1D", "1W", "1M", "ALL"];

export const PriceChart = ({ market, outcome }: { market: Market; outcome: "yes" | "no" }) => {
  const isPyth = market.resolution.mode === "pyth";
  const highlight = outcome === "yes" ? "#0f766e" : "#dc2626";
  const [range, setRange] = useState<PriceHistoryRange>("1D");
  const [history, setHistory] = useState<PriceHistoryResponse>({ range: "1D", points: [] });
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        setLoading(true);
        const { data } = await api.get<PriceHistoryResponse>(`/price-history/${market.market_id}`, {
          params: { range },
        });
        if (!active) return;
        setHistory(data);
      } catch (error) {
        console.error("load price history failed", error);
        if (active) setHistory({ range, points: [] });
      } finally {
        if (active) setLoading(false);
      }
    };

    void load();
    return () => {
      active = false;
    };
  }, [market.market_id, range]);

  useEffect(() => {
    let active = true;
    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

    const connect = () => {
      if (!active) return;
      const wsURL = buildMarketWSURL(market.market_id);
      ws = new WebSocket(wsURL);
      ws.onmessage = (event) => {
        try {
          const payload = JSON.parse(event.data) as Partial<MarketTradeSocketMessage>;
          if (payload.market_id !== market.market_id || !payload.trade_id) return;
          const point = {
            timestamp: payload.executed_at || new Date().toISOString(),
            price: payload.price_tick || "0",
            quantity: payload.match_qty || "0",
          };
          setHistory((current) => {
            if (range === "ALL") return current;
            const points = [...current.points.filter((item) => item.timestamp !== point.timestamp || item.price !== point.price), point];
            return { range: current.range, points };
          });
        } catch (error) {
          console.error("price chart websocket parse failed", error);
        }
      };
      ws.onerror = () => undefined;
      ws.onclose = () => {
        if (!active) return;
        reconnectTimer = setTimeout(connect, 1200);
      };
    };

    connect();
    return () => {
      active = false;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      ws?.close();
    };
  }, [market.market_id, range]);

  const points = useMemo(() => {
    const mapped = history.points
      .map((point) => {
        const price = Number(point.price);
        const ts = new Date(point.timestamp).getTime();
        if (!Number.isFinite(price) || Number.isNaN(ts)) return null;
        return {
          x: ts,
          y: outcome === "yes" ? price : 100 - price,
          quantity: point.quantity || "0",
          timestamp: point.timestamp,
        };
      })
      .filter((point): point is { x: number; y: number; quantity: string; timestamp: string } => point !== null);
    return mapped;
  }, [history.points, outcome]);

  const chart = useMemo(() => buildChartPath(points), [points]);
  const latestPoint = points.length > 0 ? points[points.length - 1] : null;
  const firstPoint = points.length > 0 ? points[0] : null;
  const delta = latestPoint && firstPoint ? latestPoint.y - firstPoint.y : 0;

  return (
    <section className="rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div className="mb-4 flex items-start justify-between gap-4">
        <div>
          <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">{outcome.toUpperCase()} Price</h3>
          <p className="text-sm text-zinc-500 dark:text-zinc-400">
            {isPyth ? "Pyth markets settle from oracle conditions after trigger." : "Creator markets settle by the configured authority."}
          </p>
        </div>
        <div className="text-right">
          <div className="text-3xl font-semibold text-zinc-900 dark:text-zinc-100">
            {latestPoint ? `${latestPoint.y.toFixed(1)}%` : "--"}
          </div>
          <div className={`${delta <= 0 ? "text-rose-500" : "text-emerald-500"} text-sm font-medium`}>
            {latestPoint && firstPoint ? `${delta > 0 ? "+" : ""}${delta.toFixed(1)}%` : loading ? "Loading..." : "No data"}
          </div>
        </div>
      </div>

      <div className="mb-4 overflow-hidden rounded-2xl border border-zinc-200 bg-zinc-50 dark:border-zinc-800 dark:bg-zinc-950">
        <svg viewBox="0 0 640 240" className="h-56 w-full">
          <g strokeDasharray="4 6" className="text-zinc-200 dark:text-zinc-800">
            {[0, 25, 50, 75, 100].map((value) => {
              const y = 220 - value * 2;
              return <line key={value} x1="0" y1={y} x2="640" y2={y} stroke="currentColor" strokeWidth="1" />;
            })}
          </g>
          {chart ? <path d={chart} fill="none" stroke={highlight} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" /> : null}
        </svg>
      </div>

      <div className="flex items-center justify-between gap-4">
        <div className="text-sm text-zinc-500 dark:text-zinc-400">
          {latestPoint ? `Last update ${formatTime(latestPoint.timestamp)}` : loading ? "Loading chart..." : "No trade history yet"}
        </div>
        <div className="inline-flex rounded-full border border-zinc-200 p-1 dark:border-zinc-800">
          {RANGES.map((value) => (
            <button
              key={value}
              className={`rounded-full px-3 py-1 text-sm font-medium ${
                range === value
                  ? "bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
                  : "text-zinc-500 hover:bg-zinc-100 dark:text-zinc-400 dark:hover:bg-zinc-800"
              }`}
              onClick={() => setRange(value)}
            >
              {value}
            </button>
          ))}
        </div>
      </div>
    </section>
  );
};

function buildChartPath(points: Array<{ x: number; y: number }>): string {
  if (points.length === 0) return "";
  if (points.length === 1) {
    const y = 220 - points[0].y * 2;
    return `M 0 ${y} L 640 ${y}`;
  }
  const xs = points.map((point) => point.x);
  const minX = Math.min(...xs);
  const maxX = Math.max(...xs);
  const spread = Math.max(1, maxX - minX);
  return points
    .map((point, index) => {
      const x = ((point.x - minX) / spread) * 640;
      const y = 220 - point.y * 2;
      return `${index === 0 ? "M" : "L"} ${x.toFixed(2)} ${y.toFixed(2)}`;
    })
    .join(" ");
}

function formatTime(value: string): string {
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
