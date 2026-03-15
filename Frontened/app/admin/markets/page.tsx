"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import api from "@/app/utils/axiosInstance";
import { Market, MarketsResponse } from "@/types/market";

export default function AdminMarketsPage() {
  const [markets, setMarkets] = useState<Market[]>([]);

  useEffect(() => {
    api.get<MarketsResponse>("/markets").then(({ data }) => setMarkets(data.markets || [])).catch(console.error);
  }, []);

  return (
    <div>
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-3xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">Markets</h1>
          <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">These entries come from the new Banckend v1a repository layer.</p>
        </div>
        <Link href="/markets/create" className="rounded-full bg-zinc-900 px-5 py-3 text-sm font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900">Create market</Link>
      </div>
      <div className="grid gap-4">
        {markets.map((market) => (
          <Link key={market.id} href={`/admin/markets/${market.market_id}`} className="rounded-[2rem] border border-black/5 bg-white p-6 shadow-sm transition hover:-translate-y-0.5 dark:border-white/10 dark:bg-zinc-900">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div>
                <div className="text-xs uppercase tracking-[0.25em] text-zinc-500 dark:text-zinc-400">{market.category}</div>
                <h2 className="mt-2 text-xl font-semibold text-zinc-950 dark:text-zinc-50">{market.title}</h2>
              </div>
              <span className="rounded-full bg-zinc-100 px-3 py-1 text-xs font-semibold capitalize text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">{market.status}</span>
            </div>
          </Link>
        ))}
        {markets.length === 0 ? <div className="rounded-[2rem] border border-dashed border-zinc-300 bg-white/80 p-8 text-center text-zinc-500 dark:border-zinc-700 dark:bg-zinc-900/80 dark:text-zinc-400">No markets yet.</div> : null}
      </div>
    </div>
  );
}
