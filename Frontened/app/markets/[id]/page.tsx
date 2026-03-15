"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import api from "@/app/utils/axiosInstance";
import { Market, MarketResponse } from "@/types/market";
import { PriceChart } from "@/components/market/price-chart";
import { Orderbook } from "@/components/market/orderbook";
import { SplitMergeModal } from "@/components/market/split-merge-modal";
import { UserMarketTabs } from "@/components/market/user-market-tabs";
import { UserPositions } from "@/components/market/user-positions";
import { useTrading } from "@/hooks/useTrading";
import { usePrivy } from "@privy-io/react-auth";

export default function MarketDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { user, login } = usePrivy();
  const { placeOrder, loading: tradeLoading } = useTrading();
  const [market, setMarket] = useState<Market | null>(null);
  const [loading, setLoading] = useState(true);
  const [outcome, setOutcome] = useState<"yes" | "no">("yes");
  const [action, setAction] = useState<"buy" | "sell">("buy");
  const [orderType, setOrderType] = useState<"market" | "limit">("market");
  const [amount, setAmount] = useState("");
  const [limitPrice, setLimitPrice] = useState(0.55);
  const [modal, setModal] = useState<null | "split" | "merge">(null);

  useEffect(() => {
    const load = async () => {
      try {
        const { data } = await api.get<MarketResponse>(`/markets/${id}`);
        setMarket(data.market);
      } finally {
        setLoading(false);
      }
    };
    load().catch(console.error);
  }, [id]);

  if (loading) {
    return <div className="mx-auto max-w-6xl px-4 py-10">Loading market...</div>;
  }
  if (!market) {
    return <div className="mx-auto max-w-6xl px-4 py-10">Market not found.</div>;
  }

  return (
    <div className="min-h-screen bg-stone-100 dark:bg-zinc-950">
      <main className="mx-auto max-w-6xl px-4 py-10 sm:px-6 lg:px-8">
        <Link href="/" className="text-sm font-medium text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100">← Back to markets</Link>
        <div className="mt-6 grid gap-8 lg:grid-cols-[1.2fr_0.8fr]">
          <div className="space-y-6">
            <section className="rounded-[2rem] border border-black/5 bg-white p-8 shadow-sm dark:border-white/10 dark:bg-zinc-900">
              <div className="flex flex-wrap items-center gap-3">
                <span className={`rounded-full px-3 py-1 text-xs font-semibold ${market.resolution.mode === "pyth" ? "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-200" : "bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200"}`}>
                  {market.resolution.mode === "pyth" ? "Pyth Secured" : "Creator Resolved"}
                </span>
                {market.category ? (
                  <span className="rounded-full bg-zinc-100 px-3 py-1 text-xs font-semibold text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">
                    {market.category}
                  </span>
                ) : null}
              </div>
              <h1 className="mt-4 text-4xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">{market.title}</h1>
              <p className="mt-4 max-w-3xl text-zinc-600 dark:text-zinc-300">{market.description || "No extra rules provided."}</p>
              <div className="mt-6 grid gap-4 md:grid-cols-2">
                <DetailCard label="Close time" value={new Date(market.close_time).toLocaleString()} />
                <DetailCard label="Collateral mint" value={market.collateral_mint} mono />
                {market.resolution.mode === "creator" ? (
                  <DetailCard label="Resolution authority" value={market.resolution.authority || "not set"} mono />
                ) : (
                  <>
                    <DetailCard label="Oracle feed" value={market.resolution.oracle_feed || "not set"} mono />
                    <DetailCard label="Oracle threshold" value={`${market.resolution.oracle_condition || "gte"} ${market.resolution.oracle_target_price || 0}`} />
                  </>
                )}
              </div>
              {market.resolution.mode === "creator" ? (
                <div className="mt-6 rounded-2xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-900/20 dark:text-amber-100">This market is creator resolved. Traders should evaluate creator reputation before treating the outcome as trust-minimized.</div>
              ) : (
                <div className="mt-6 rounded-2xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-900 dark:border-emerald-900/60 dark:bg-emerald-900/20 dark:text-emerald-100">This market stores a Pyth feed, a threshold condition, and an oracle observation time in the contract state.</div>
              )}
            </section>
            <PriceChart market={market} outcome={outcome} />
            <Orderbook outcome={outcome} marketId={market.market_id} />
            <UserPositions market={market} />
            <UserMarketTabs marketId={market.market_id} />
          </div>

          <aside className="lg:sticky lg:top-8 lg:h-fit">
            <div className="rounded-[2rem] border border-black/5 bg-white p-6 shadow-sm dark:border-white/10 dark:bg-zinc-900">
              <div className="flex gap-2 rounded-full bg-zinc-100 p-1 dark:bg-zinc-800">
                <button className={`flex-1 rounded-full px-4 py-2 text-sm font-semibold ${action === "buy" ? "bg-white text-zinc-950 shadow-sm dark:bg-zinc-700 dark:text-zinc-50" : "text-zinc-500"}`} onClick={() => setAction("buy")}>Buy</button>
                <button className={`flex-1 rounded-full px-4 py-2 text-sm font-semibold ${action === "sell" ? "bg-white text-zinc-950 shadow-sm dark:bg-zinc-700 dark:text-zinc-50" : "text-zinc-500"}`} onClick={() => setAction("sell")}>Sell</button>
              </div>
              <div className="mt-4 grid grid-cols-2 gap-2">
                {(["yes", "no"] as const).map((item) => (
                  <button key={item} className={`rounded-2xl border px-4 py-3 text-sm font-semibold ${outcome === item ? "border-zinc-900 bg-zinc-900 text-white dark:border-zinc-100 dark:bg-zinc-100 dark:text-zinc-900" : "border-zinc-200 bg-white text-zinc-700 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200"}`} onClick={() => setOutcome(item)}>{item.toUpperCase()}</button>
                ))}
              </div>
              <div className="mt-4 grid grid-cols-2 gap-2">
                {(["market", "limit"] as const).map((item) => (
                  <button key={item} className={`rounded-2xl border px-4 py-2 text-sm font-medium ${orderType === item ? "border-emerald-600 text-emerald-700 dark:text-emerald-300" : "border-zinc-200 text-zinc-500 dark:border-zinc-700 dark:text-zinc-300"}`} onClick={() => setOrderType(item)}>{item}</button>
                ))}
              </div>
              <div className="mt-4 space-y-3">
                <input className="w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 outline-none dark:border-zinc-700 dark:bg-zinc-950" placeholder={orderType === "limit" ? "Shares" : "Amount"} value={amount} onChange={(event) => setAmount(event.target.value)} />
                {orderType === "limit" ? <input className="w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 outline-none dark:border-zinc-700 dark:bg-zinc-950" placeholder="Limit price" type="number" value={limitPrice} onChange={(event) => setLimitPrice(Number(event.target.value))} /> : null}
              </div>
              <button
                className="mt-4 w-full rounded-2xl bg-zinc-900 px-4 py-3 text-sm font-semibold text-white disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900"
                disabled={!user || !amount || tradeLoading}
                onClick={() => placeOrder({ market, action, outcome, orderType, amount, limitPrice })}
              >
                {tradeLoading ? "Submitting..." : `${action === "buy" ? "Buy" : "Sell"} ${outcome.toUpperCase()}`}
              </button>
              {!user ? <button className="mt-3 w-full rounded-2xl border border-zinc-300 px-4 py-3 text-sm font-semibold dark:border-zinc-700" onClick={login}>Connect wallet to trade</button> : null}
              <div className="mt-4 grid grid-cols-2 gap-3">
                <button className="rounded-2xl border border-zinc-300 px-4 py-3 text-sm font-semibold dark:border-zinc-700" onClick={() => setModal("split")}>Split</button>
                <button className="rounded-2xl border border-zinc-300 px-4 py-3 text-sm font-semibold dark:border-zinc-700" onClick={() => setModal("merge")}>Merge</button>
              </div>
              <button className="mt-3 w-full rounded-2xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white" onClick={() => placeOrder({ market, action: "buy", outcome: "yes", orderType: "claim", amount: "0", limitPrice: 0 })}>Claim winnings</button>
              <p className="mt-4 text-xs leading-6 text-zinc-500 dark:text-zinc-400">Buy/sell calls the future v1b matching API today. Split/merge/claim already hit the new Banckend transaction-envelope routes and surface pending tx-builder gaps without breaking UX.</p>
            </div>
          </aside>
        </div>
      </main>
      <SplitMergeModal isOpen={modal !== null} onClose={() => setModal(null)} type={modal ?? "split"} market={market} />
    </div>
  );
}

const DetailCard = ({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) => (
  <div className="rounded-2xl bg-zinc-50 p-4 dark:bg-zinc-800/60">
    <div className="text-xs uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">{label}</div>
    <div className={`mt-2 text-sm text-zinc-900 dark:text-zinc-100 ${mono ? "break-all font-mono" : "font-medium"}`}>{value}</div>
  </div>
);
