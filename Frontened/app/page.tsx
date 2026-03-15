"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import api from "@/app/utils/axiosInstance";
import { MarketsResponse, Market } from "@/types/market";
import { ThemeToggle } from "@/components/theme-toggle";
import { UserMenu } from "@/components/user-menu";
import { usePrivy } from "@privy-io/react-auth";
import { useUSDCBalance } from "@/hooks/useUSDCBalance";
import { getIdentityToken } from "@privy-io/react-auth";
import { toast } from "sonner";

export default function HomePage() {
  const { user, login } = usePrivy();
  const { balance } = useUSDCBalance();
  const [markets, setMarkets] = useState<Market[]>([]);
  const [loading, setLoading] = useState(true);
  const [faucetLoading, setFaucetLoading] = useState(false);

  useEffect(() => {
    const load = async () => {
      try {
        const { data } = await api.get<MarketsResponse>("/markets");
        setMarkets(data.markets || []);
      } finally {
        setLoading(false);
      }
    };
    load().catch(console.error);
  }, []);

  const items = useMemo(() => markets, [markets]);

  return (
    <div className="min-h-screen bg-[radial-gradient(circle_at_top,_rgba(16,185,129,0.12),_transparent_35%),linear-gradient(180deg,#f5f1e8_0%,#f7f7f5_40%,#fafafa_100%)] dark:bg-[radial-gradient(circle_at_top,_rgba(16,185,129,0.16),_transparent_35%),linear-gradient(180deg,#09090b_0%,#111827_100%)]">
      <header className="sticky top-0 z-20 border-b border-black/5 bg-white/70 backdrop-blur-xl dark:border-white/10 dark:bg-zinc-950/70">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-4 py-4 sm:px-6 lg:px-8">
          <div>
            <div className="text-xs uppercase tracking-[0.3em] text-emerald-700 dark:text-emerald-300">BlinkPredict</div>
            <h1 className="text-xl font-semibold">v1a Skeleton Markets</h1>
          </div>
          <div className="flex items-center gap-3">
            <ThemeToggle />
            {user ? (
              <div className="flex items-center gap-3">
                <div className="rounded-full bg-zinc-900 px-3 py-1 text-xs font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900">{balance} vUSDC</div>
                <UserMenu />
              </div>
            ) : (
              <button className="rounded-full bg-zinc-900 px-4 py-2 text-sm font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900" onClick={login}>Connect</button>
            )}
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-4 py-10 sm:px-6 lg:px-8">
        <section className="mb-10 grid gap-8 lg:grid-cols-[1.3fr_0.7fr]">
          <div className="rounded-[2rem] border border-black/5 bg-white/85 p-8 shadow-sm backdrop-blur dark:border-white/10 dark:bg-zinc-900/80">
            <p className="text-sm font-medium text-zinc-500 dark:text-zinc-400">Chain-offload matching stays disabled in v1a, but the full product shell is ready for split / merge / claim and dual resolution-mode markets.</p>
            <h2 className="mt-4 max-w-2xl text-4xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">A Solana prediction market shell built for creator-resolved and Pyth-secured markets.</h2>
            <div className="mt-6 flex flex-wrap gap-3">
              <Link className="rounded-full bg-emerald-600 px-5 py-3 text-sm font-semibold text-white" href="/markets/create">Create market</Link>
              {user ? (
                <button
                  className="rounded-full border border-zinc-300 px-5 py-3 text-sm font-semibold text-zinc-700 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200"
                  disabled={faucetLoading}
                  onClick={async () => {
                    try {
                      setFaucetLoading(true);
                      const token = await getIdentityToken();
                      if (!token) {
                        toast.error("Not authenticated", { description: "Please login again." });
                        return;
                      }
                      const { data } = await api.post("/faucet/claim", {}, { headers: { "privy-id-token": token } });
                      toast.success("Faucet submitted", { description: data.signature });
                    } catch (error: unknown) {
                      const response =
                        typeof error === "object" && error !== null && "response" in error
                          ? (error as { response?: { data?: { message?: string; next_allowed_at?: string } } }).response
                          : undefined;
                      const message = response?.data?.message || (error instanceof Error ? error.message : "Faucet failed");
                      const next = response?.data?.next_allowed_at;
                      toast.error(message, next ? { description: `Next allowed at: ${next}` } : undefined);
                    } finally {
                      setFaucetLoading(false);
                    }
                  }}
                  type="button"
                >
                  {faucetLoading ? "Claiming..." : "Claim 500 vUSDC"}
                </button>
              ) : null}
              <Link className="rounded-full border border-zinc-300 px-5 py-3 text-sm font-semibold text-zinc-700 dark:border-zinc-700 dark:text-zinc-200" href="/profile">Portfolio shell</Link>
            </div>
          </div>
          <div className="rounded-[2rem] border border-black/5 bg-[#111827] p-8 text-white shadow-sm dark:border-white/10">
            <div className="text-xs uppercase tracking-[0.3em] text-emerald-300">v1a status</div>
            <ul className="mt-6 space-y-4 text-sm text-zinc-200">
              <li>Market discovery, admin creation, and market detail pages are wired to the new Banckend API.</li>
              <li>Split / merge / claim hit the new transaction-envelope endpoints and safely expose pending builder gaps.</li>
              <li>Orderbook, trades, and open orders already use the future v1b interface contract.</li>
            </ul>
          </div>
        </section>

        {loading ? (
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
            {Array.from({ length: 3 }).map((_, index) => (
              <div key={index} className="h-64 animate-pulse rounded-[2rem] bg-white/80 dark:bg-zinc-900/80" />
            ))}
          </div>
        ) : items.length === 0 ? (
          <div className="rounded-[2rem] border border-dashed border-zinc-300 bg-white/80 p-10 text-center text-zinc-500 dark:border-zinc-700 dark:bg-zinc-900/80 dark:text-zinc-400">No markets yet. Create one from the admin surface.</div>
        ) : (
          <div className="grid gap-5 md:grid-cols-2 xl:grid-cols-3">
            {items.map((market) => (
              <Link key={market.id} href={`/markets/${market.market_id}`} className="group rounded-[2rem] border border-black/5 bg-white/90 p-6 shadow-sm transition hover:-translate-y-1 hover:shadow-xl dark:border-white/10 dark:bg-zinc-900/85">
                <div className="flex items-start justify-between gap-4">
                  <div>
                    <h3 className="mt-3 text-xl font-semibold text-zinc-950 transition group-hover:text-emerald-700 dark:text-zinc-50 dark:group-hover:text-emerald-300">{market.title}</h3>
                  </div>
                  <ResolutionBadge market={market} />
                </div>
                <p className="mt-4 line-clamp-3 text-sm text-zinc-600 dark:text-zinc-300">{market.description || "No resolution notes yet."}</p>
                <div className="mt-6 flex items-center justify-between border-t border-zinc-200 pt-4 text-sm text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
                  <span>Ends {new Date(market.close_time).toLocaleDateString()}</span>
                  <span className="capitalize">{market.status}</span>
                </div>
              </Link>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}

const ResolutionBadge = ({ market }: { market: Market }) => {
  if (market.resolution.mode === "pyth") {
    return <span className="rounded-full bg-emerald-100 px-3 py-1 text-xs font-semibold text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-200">Pyth Secured</span>;
  }
  return <span className="rounded-full bg-amber-100 px-3 py-1 text-xs font-semibold text-amber-800 dark:bg-amber-900/40 dark:text-amber-200">Creator Resolved</span>;
};
