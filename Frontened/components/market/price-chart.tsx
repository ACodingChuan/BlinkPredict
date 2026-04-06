"use client";

import { useMemo, useState } from "react";
import { Market, PriceHistoryRange, PricePoint } from "@/types/market";

const RANGES: PriceHistoryRange[] = ["1H", "6H", "1D", "1W", "1M", "ALL"];

export const PriceChart = ({
  market,
  outcome,
  points,
  socketState,
  loading,
}: {
  market: Market;
  outcome: "yes" | "no";
  points: PricePoint[];
  socketState: "connecting" | "live" | "offline";
  loading: boolean;
}) => {
  const isPyth = market.resolution.mode === "pyth";
  const highlight = outcome === "yes" ? "#0f766e" : "#dc2626";
  const [range, setRange] = useState<PriceHistoryRange>("1D");

  const filteredPoints = useMemo(() => {
    const start = rangeStart(range);
    return points
      .filter((point) => {
        if (range === "ALL") return true;
        const ts = new Date(point.timestamp).getTime();
        return Number.isFinite(ts) && ts >= start;
      })
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
  }, [outcome, points, range]);

  const chart = useMemo(() => buildChartPath(filteredPoints), [filteredPoints]);
  const latestPoint = filteredPoints.length > 0 ? filteredPoints[filteredPoints.length - 1] : null;
  const firstPoint = filteredPoints.length > 0 ? filteredPoints[0] : null;
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
            {latestPoint && firstPoint ? `${delta > 0 ? "+" : ""}${delta.toFixed(1)}%` : loading ? "Waiting for snapshot..." : "No data"}
          </div>
          <div className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
            market stream {socketState === "live" ? "live" : socketState}
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
          {latestPoint ? `Last update ${formatTime(latestPoint.timestamp)}` : loading ? "Waiting for price history snapshot..." : "No trade history yet"}
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

function rangeStart(range: PriceHistoryRange): number {
  const now = Date.now();
  switch (range) {
    case "1H":
      return now - 60 * 60 * 1000;
    case "6H":
      return now - 6 * 60 * 60 * 1000;
    case "1D":
      return now - 24 * 60 * 60 * 1000;
    case "1W":
      return now - 7 * 24 * 60 * 60 * 1000;
    case "1M":
      return now - 30 * 24 * 60 * 60 * 1000;
    case "ALL":
    default:
      return 0;
  }
}
