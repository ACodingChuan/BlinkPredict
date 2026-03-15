"use client";

import { Market } from "@/types/market";

export const PriceChart = ({ market, outcome }: { market: Market; outcome: "yes" | "no" }) => {
  const isPyth = market.resolution.mode === "pyth";
  const highlight = outcome === "yes" ? "#0f766e" : "#dc2626";
  const points = outcome === "yes"
    ? "0,80 40,70 80,62 120,66 160,55 200,58 240,49 280,46 320,44"
    : "0,20 40,30 80,38 120,34 160,45 200,42 240,51 280,54 320,56";

  return (
    <section className="rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div className="mb-4 flex items-start justify-between gap-4">
        <div>
          <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">{outcome.toUpperCase()} Price Preview</h3>
          <p className="text-sm text-zinc-500 dark:text-zinc-400">
            {isPyth ? "Pyth 市场会在触发结算后由预言机决定最终 outcome。" : "Creator 市场由创建者地址手动决议。"}
          </p>
        </div>
        <span className="rounded-full border border-zinc-200 px-3 py-1 text-xs font-semibold text-zinc-600 dark:border-zinc-700 dark:text-zinc-300">
          skeleton data
        </span>
      </div>
      <svg viewBox="0 0 320 100" className="h-40 w-full overflow-visible">
        <polyline points={points} fill="none" stroke={highlight} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    </section>
  );
};
