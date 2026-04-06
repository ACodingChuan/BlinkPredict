"use client";

import { TradeItem } from "@/types/market";

export const RecentTrades = ({
  trades,
  loading,
  matchingEnabled,
  socketState,
}: {
  trades: TradeItem[];
  loading: boolean;
  matchingEnabled: boolean;
  socketState: "connecting" | "live" | "offline";
}) => {
  if (trades.length === 0) {
    return (
      <div className="rounded-2xl border border-dashed border-zinc-300 px-4 py-8 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
        {loading ? "Waiting for trade snapshot..." : matchingEnabled ? "No trades yet." : "No trades yet (matcher still in rollout)."}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto rounded-2xl border border-zinc-200 dark:border-zinc-800">
      <div className="flex items-center justify-between border-b border-zinc-200 bg-zinc-50 px-4 py-2 text-xs text-zinc-500 dark:border-zinc-800 dark:bg-zinc-900/60 dark:text-zinc-400">
        <span>Trade stream</span>
        <span>{socketState === "live" ? "pusher live" : `pusher ${socketState}`}</span>
      </div>
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
  return (parsed / 100).toLocaleString(undefined, { minimumFractionDigits: 0, maximumFractionDigits: 2 });
}

function formatTime(value?: string): string {
  if (!value) return "--";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString();
}
