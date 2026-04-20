"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import api from "@/app/utils/axiosInstance";
import { MarketsResponse, Market } from "@/types/market";
import { usePrivy } from "@/lib/auth-client";
import { useUSDCStore } from "@/store/usdcStore";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
import { toast } from "sonner";

export default function HomePage() {
  const { user, authenticated, getAccessToken } = usePrivy();
  const { walletAddress } = useCurrentSolanaWallet();
  const walletBalance = useUSDCStore((state) => state.balance);
  const walletBalanceLoading = useUSDCStore((state) => state.loading || state.isRefreshing);
  const syncWalletBalance = useUSDCStore((state) => state.syncBalance);
  const [markets, setMarkets] = useState<Market[]>([]);
  const [loading, setLoading] = useState(true);
  const [fetchError, setFetchError] = useState("");
  const [faucetLoading, setFaucetLoading] = useState(false);

  useEffect(() => {
    const load = async () => {
      try {
        const { data } = await api.get<MarketsResponse>("/markets");
        setMarkets(data.markets || []);
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : "Failed to load markets";
        setFetchError(msg);
      } finally {
        setLoading(false);
      }
    };
    void load();
  }, []);

  useEffect(() => {
    if (!walletAddress || !authenticated) {
      return;
    }
    void syncWalletBalance(walletAddress);
  }, [authenticated, syncWalletBalance, walletAddress]);

  const items = useMemo(() => markets, [markets]);

  return (
    <div className="min-h-screen bg-[radial-gradient(circle_at_top,_rgba(16,185,129,0.12),_transparent_35%),linear-gradient(180deg,#f5f1e8_0%,#f7f7f5_40%,#fafafa_100%)] dark:bg-[radial-gradient(circle_at_top,_rgba(16,185,129,0.16),_transparent_35%),linear-gradient(180deg,#09090b_0%,#111827_100%)]">
      <main className="mx-auto max-w-6xl px-4 py-10 sm:px-6 lg:px-8">
        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_22rem] lg:items-start">
          <section className="space-y-6">
            <div className="rounded-[2rem] border border-black/5 bg-white/85 p-7 shadow-sm backdrop-blur dark:border-white/10 dark:bg-zinc-900/80">
              <h2 className="max-w-2xl text-4xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">A Solana prediction market shell built for creator-resolved and Pyth-secured markets.</h2>
              <div className="mt-6 flex flex-wrap gap-3">
                <Link className="rounded-full bg-emerald-600 px-5 py-3 text-sm font-semibold text-white" href="/markets/create">Create market</Link>
                {authenticated && user ? (
                  <button
                    className="rounded-full border border-zinc-300 px-5 py-3 text-sm font-semibold text-zinc-700 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200"
                    disabled={faucetLoading}
                    onClick={async () => {
                      try {
                        setFaucetLoading(true);
                        const token = await getAccessToken();
                        if (!token) {
                          toast.error("Not authenticated", { description: "Please login again." });
                          return;
                        }
                        const { data } = await api.post(
                          "/faucet/claim",
                          { wallet_address: user.walletAddress },
                          { headers: { Authorization: `Bearer ${token}` } },
                        );
                        toast.success("Faucet submitted", { description: data.signature });
                        if (walletAddress) {
                          void syncWalletBalance(walletAddress);
                        }
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
                    {faucetLoading ? "Claiming..." : "Faucet (500 vUSDC)"}
                  </button>
                ) : null}
              </div>
              {authenticated && user ? (
                <div className="mt-5 grid gap-3 sm:grid-cols-2">
                  <BalanceCard
                    label="Wallet vUSDC (RPC)"
                    value={walletBalanceLoading ? "Refreshing..." : `${walletBalance} vUSDC`}
                  />
                  <BalanceCard label="Connected wallet" value={user.walletAddress} mono />
                </div>
              ) : null}
            </div>

            {loading ? (
              <div className="grid gap-4 md:grid-cols-2">
                {Array.from({ length: 3 }).map((_, index) => (
                  <div key={index} className="h-64 animate-pulse rounded-[2rem] bg-white/80 dark:bg-zinc-900/80" />
                ))}
              </div>
            ) : fetchError ? (
              <div className="rounded-[2rem] border border-rose-200 bg-rose-50 p-10 text-center text-rose-700 dark:border-rose-900/50 dark:bg-rose-900/20 dark:text-rose-300">
                Failed to load markets: {fetchError}
              </div>
            ) : items.length === 0 ? (
              <div className="rounded-[2rem] border border-dashed border-zinc-300 bg-white/80 p-10 text-center text-zinc-500 dark:border-zinc-700 dark:bg-zinc-900/80 dark:text-zinc-400">No markets yet. Create one from the admin surface.</div>
            ) : (
              <div className="grid gap-5 md:grid-cols-2">
                {items.map((market) => (
                  <Link key={market.id} href={`/markets/${market.market_id}`} className="group rounded-[2rem] border border-black/5 bg-white/90 p-6 shadow-sm transition hover:-translate-y-1 hover:shadow-xl dark:border-white/10 dark:bg-zinc-900/85">
                    <div className="flex items-start justify-between gap-4">
                      <div>
                        <h3 className="mt-3 text-xl font-semibold text-zinc-950 transition group-hover:text-emerald-700 dark:text-zinc-50 dark:group-hover:text-emerald-300">{market.title}</h3>
                      </div>
                      <ResolutionBadge market={market} />
                    </div>
                    <p className="mt-4 line-clamp-3 text-sm text-zinc-600 dark:text-zinc-300">{market.description || "No resolution notes yet."}</p>
                    <div className="mt-6 space-y-2 border-t border-zinc-200 pt-4 text-sm text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
                      <div className="flex items-center justify-between">
                        <span>Ends {new Date(market.close_time).toLocaleDateString()}</span>
                        <span className="capitalize">{market.status}</span>
                      </div>
                      <div className="flex items-center justify-between">
                        <span>Claim by</span>
                        <span>{market.claim_deadline_time ? new Date(market.claim_deadline_time).toLocaleDateString() : "N/A"}</span>
                      </div>
                    </div>
                  </Link>
                ))}
              </div>
            )}
          </section>

          <aside className="lg:sticky lg:top-8">
            <section className="rounded-[2rem] border border-black/10 bg-white p-4 text-zinc-900 shadow-sm dark:border-white/10 dark:bg-zinc-900 dark:text-zinc-100">
              <h3 className="text-[1.75rem] font-semibold tracking-tight">Contact</h3>
              <p className="mt-2 max-w-sm text-[14px] leading-6 text-zinc-600 dark:text-zinc-300">
                I&apos;m currently looking for a Web3 role (full-stack / smart contracts / backend). If you&apos;d like to offer me an interview opportunity, please contact me.
              </p>

              <div className="mt-5 space-y-3.5">
                <ContactItem icon="mail" label="Email" value="yznt7381@hotmail.com" />
                <ContactItem icon="message" label="Telegram" value="t.me/KopChuan" />
              </div>

              <div className="mt-4 text-[10px] font-semibold uppercase tracking-[0.18em] text-zinc-500 dark:text-zinc-400">Demo</div>
              <div className="mt-2 space-y-2">
                <ExternalAction
                  href="https://youtu.be/eP1H5tq1KOM"
                  label="YouTube Demo"
                  description="Project walkthrough on YouTube"
                />
                <ExternalAction
                  href="https://www.bilibili.com/video/BV1zfQuBsEr8/"
                  label="Bilibili Demo"
                  description="Project walkthrough on Bilibili"
                />
              </div>

              <div className="mt-4 text-[10px] font-semibold uppercase tracking-[0.18em] text-zinc-500 dark:text-zinc-400">Resume</div>
              <div className="mt-2 space-y-2">
                <DownloadAction href="/resume/resumezh-CN.pdf" label="Download Resume (中文)" />
                <DownloadAction href="/resume/resume-en.pdf" label="Download Resume (EN)" />
              </div>
            </section>
          </aside>
        </div>
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

const ContactItem = ({ icon, label, value }: { icon: "mail" | "message"; label: string; value: string }) => (
  <div className="flex items-start gap-2.5">
    <div className="mt-0.5 flex h-7 w-7 items-center justify-center rounded-full bg-blue-50 text-blue-500 dark:bg-blue-500/10 dark:text-blue-300">
      <ContactIcon kind={icon} />
    </div>
    <div>
      <div className="text-[13px] font-semibold text-zinc-500 dark:text-zinc-400">{label}</div>
      <div className="mt-0.5 text-base font-semibold tracking-tight text-zinc-900 dark:text-zinc-100">{value}</div>
    </div>
  </div>
);

const DownloadAction = ({ href, label }: { href: string; label: string }) => (
  <a
    className="flex w-full items-center justify-center gap-2 rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-2.5 text-[14px] font-semibold text-zinc-800 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
    href={href}
    download
  >
    <ContactIcon kind="download" />
    {label}
  </a>
);

const ExternalAction = ({
  href,
  label,
  description,
}: {
  href: string;
  label: string;
  description: string;
}) => (
  <a
    className="group flex w-full items-center justify-between gap-3 rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 text-zinc-900 transition hover:border-emerald-300 hover:bg-emerald-50 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100 dark:hover:border-emerald-700 dark:hover:bg-emerald-950/30"
    href={href}
    target="_blank"
    rel="noreferrer"
  >
    <div>
      <div className="text-[14px] font-semibold">{label}</div>
      <div className="mt-0.5 text-[12px] text-zinc-500 dark:text-zinc-400">{description}</div>
    </div>
    <span className="inline-flex h-9 w-9 items-center justify-center rounded-full bg-white text-zinc-700 shadow-sm transition group-hover:text-emerald-700 dark:bg-zinc-900 dark:text-zinc-200 dark:group-hover:text-emerald-300">
      <ContactIcon kind="external" />
    </span>
  </a>
);

const ContactIcon = ({ kind }: { kind: "mail" | "message" | "download" | "external" }) => {
  if (kind === "mail") {
    return (
      <svg aria-hidden="true" className="h-4.5 w-4.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
        <path d="M4 6h16v12H4z" />
        <path d="m4 7 8 6 8-6" />
      </svg>
    );
  }
  if (kind === "message") {
    return (
      <svg aria-hidden="true" className="h-4.5 w-4.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
        <path d="M21 11.5a8.5 8.5 0 0 1-8.5 8.5H8l-4 3 1.5-4.5A8.5 8.5 0 1 1 21 11.5Z" />
      </svg>
    );
  }
  if (kind === "external") {
    return (
      <svg aria-hidden="true" className="h-4.5 w-4.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
        <path d="M14 5h5v5" />
        <path d="M10 14 19 5" />
        <path d="M19 14v4a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h4" />
      </svg>
    );
  }
  return (
    <svg aria-hidden="true" className="h-4.5 w-4.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
      <path d="M12 3v12" />
      <path d="m7 10 5 5 5-5" />
      <path d="M5 21h14" />
    </svg>
  );
};

const BalanceCard = ({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) => (
  <div className="rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 text-sm dark:border-zinc-800 dark:bg-zinc-950">
    <div className="text-xs uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">{label}</div>
    <div className={`mt-2 text-base font-semibold text-zinc-900 dark:text-zinc-100 ${mono ? "break-all font-mono text-sm" : ""}`}>{value}</div>
  </div>
);
