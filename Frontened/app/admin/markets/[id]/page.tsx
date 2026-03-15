"use client";

import { useEffect, useState } from "react";
import { useParams } from "next/navigation";
import api from "@/app/utils/axiosInstance";
import { Market, MarketResponse } from "@/types/market";
import { useIdentityToken } from "@privy-io/react-auth";

export default function AdminMarketDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { identityToken } = useIdentityToken();
  const [market, setMarket] = useState<Market | null>(null);
  const [loading, setLoading] = useState(true);
  const [message, setMessage] = useState("");

  useEffect(() => {
    api.get<MarketResponse>(`/markets/${id}`).then(({ data }) => setMarket(data.market)).finally(() => setLoading(false));
  }, [id]);

  if (loading) return <div>Loading market...</div>;
  if (!market) return <div>Market not found.</div>;

  return (
    <div className="grid gap-6 lg:grid-cols-[1.15fr_0.85fr]">
      <section className="rounded-[2rem] border border-black/5 bg-white p-8 shadow-sm dark:border-white/10 dark:bg-zinc-900">
        <div className="text-xs uppercase tracking-[0.3em] text-zinc-500 dark:text-zinc-400">Admin market detail</div>
        <h1 className="mt-3 text-3xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">{market.title}</h1>
        <div className="mt-6 grid gap-4 md:grid-cols-2">
          <Card label="Market ID" value={String(market.market_id)} mono />
          <Card label="Status" value={market.status} />
          <Card label="Mode" value={market.resolution.mode} />
          <Card label="Outcome" value={market.outcome} />
          <Card label="Market PDA" value={market.market_pda} mono />
          <Card label="Metadata URL" value={market.metadata_url} mono />
        </div>
        {message ? <div className="mt-6 rounded-2xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-800 dark:border-emerald-900/50 dark:bg-emerald-900/20 dark:text-emerald-200">{message}</div> : null}
      </section>
      <aside className="rounded-[2rem] border border-black/5 bg-white p-8 shadow-sm dark:border-white/10 dark:bg-zinc-900">
        <h2 className="text-xl font-semibold text-zinc-950 dark:text-zinc-50">Resolution actions</h2>
        <p className="mt-2 text-sm text-zinc-500 dark:text-zinc-400">Creator resolution mutates the Banckend repository state now. Pyth resolution keeps the trigger endpoint visible for the future on-chain oracle path.</p>
        <div className="mt-6 space-y-3">
          <button className="w-full rounded-2xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white" onClick={() => resolveCreator("yes", market.market_id, identityToken, setMessage, setMarket)}>Resolve YES</button>
          <button className="w-full rounded-2xl bg-rose-600 px-4 py-3 text-sm font-semibold text-white" onClick={() => resolveCreator("no", market.market_id, identityToken, setMessage, setMarket)}>Resolve NO</button>
          {market.resolution.mode === "pyth" ? <button className="w-full rounded-2xl border border-zinc-300 px-4 py-3 text-sm font-semibold dark:border-zinc-700" onClick={() => triggerPyth(market.market_id, identityToken, setMessage)}>Trigger Pyth Resolve</button> : null}
        </div>
      </aside>
    </div>
  );
}

async function resolveCreator(outcome: "yes" | "no", marketId: number, identityToken: string | null, setMessage: (value: string) => void, setMarket: (market: Market) => void) {
  const { data } = await api.post(`/admin/markets/${marketId}/resolve`, { outcome }, { headers: { "privy-id-token": identityToken } });
  setMessage(data.message);
  setMarket(data.market);
}

async function triggerPyth(marketId: number, identityToken: string | null, setMessage: (value: string) => void) {
  const { data } = await api.post(`/admin/markets/${marketId}/trigger-oracle-resolve`, {}, { headers: { "privy-id-token": identityToken } });
  setMessage(data.message);
}

const Card = ({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) => (
  <div className="rounded-2xl bg-zinc-50 p-4 dark:bg-zinc-800/60">
    <div className="text-xs uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">{label}</div>
    <div className={`mt-2 text-sm text-zinc-900 dark:text-zinc-100 ${mono ? "break-all font-mono" : "capitalize"}`}>{value}</div>
  </div>
);
