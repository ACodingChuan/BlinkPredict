"use client";

import { Market } from "@/types/market";

export const UserPositions = ({ market }: { market: Market }) => {
  return (
    <section className="rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Position Summary</h3>
      <div className="mt-4 grid gap-4 md:grid-cols-2">
        <div className="rounded-2xl bg-zinc-50 p-4 dark:bg-zinc-800/60">
          <div className="text-xs uppercase tracking-wide text-zinc-500 dark:text-zinc-400">Resolution mode</div>
          <div className="mt-1 text-sm text-zinc-800 dark:text-zinc-100">{market.resolution.mode}</div>
        </div>
        <div className="rounded-2xl bg-zinc-50 p-4 dark:bg-zinc-800/60">
          <div className="text-xs uppercase tracking-wide text-zinc-500 dark:text-zinc-400">Metadata CID</div>
          <div className="mt-1 break-all font-mono text-sm text-zinc-800 dark:text-zinc-100">{market.metadata_cid || "-"}</div>
        </div>
      </div>
    </section>
  );
};
