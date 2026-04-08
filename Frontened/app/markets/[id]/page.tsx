"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { usePrivy } from "@/lib/auth-client";
import { toast } from "sonner";
import api from "@/app/utils/axiosInstance";
import { Orderbook } from "@/components/market/orderbook";
import { PriceChart } from "@/components/market/price-chart";
import { UserMarketTabs } from "@/components/market/user-market-tabs";
import { useTrading } from "@/hooks/useTrading";
import { useMarketPublicFeed } from "@/hooks/useMarketPublicFeed";
import { useUSDCBalance } from "@/hooks/useUSDCBalance";
import { useMarketPosition } from "@/hooks/useMarketPosition";
import { Market, MarketMetadataDoc, MarketResponse } from "@/types/market";

type TradeOrderType = "market" | "limit" | "split" | "merge";

export default function MarketDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { user, login, getAccessToken } = usePrivy();
  const { syncAfterMutation: syncTradingBalance } = useUSDCBalance({ autoFetch: false });
  const { position, loading: positionLoading, syncAfterMutation: syncPosition } = useMarketPosition(id);
  const { placeOrder, loading: tradeLoading } = useTrading();
  const marketFeed = useMarketPublicFeed(id);

  const [market, setMarket] = useState<Market | null>(null);
  const [metadata, setMetadata] = useState<MarketMetadataDoc | null>(null);
  const [loading, setLoading] = useState(true);

  const [action, setAction] = useState<"buy" | "sell">("buy");
  const [outcome, setOutcome] = useState<"yes" | "no">("yes");
  const [orderType, setOrderType] = useState<TradeOrderType>("limit");
  const [showOrderTypeMenu, setShowOrderTypeMenu] = useState(false);
  const [limitPrice, setLimitPrice] = useState(0.56);
  const [tradeInput, setTradeInput] = useState("");
  const [expiryInput, setExpiryInput] = useState(() => defaultLimitExpiryLocal());
  const [lastOrderID, setLastOrderID] = useState("");

  useEffect(() => {
    const load = async () => {
      try {
        const { data } = await api.get<MarketResponse>(`/markets/${id}`);
        setMarket(data.market);
      } finally {
        setLoading(false);
      }
    };
    void load();
  }, [id]);

  useEffect(() => {
    const loadMetadata = async () => {
      if (!market?.metadata_url) {
        setMetadata(null);
        return;
      }
      try {
        const response = await fetch(`/api/market-metadata?url=${encodeURIComponent(market.metadata_url)}`);
        if (!response.ok) return;
        const payload = (await response.json()) as { data?: MarketMetadataDoc };
        setMetadata(payload.data || null);
      } catch (error) {
        console.error("metadata load failed", error);
      }
    };

    void loadMetadata();
  }, [market?.metadata_url]);

  const yesPrice = clampCent(limitPrice);
  const yesCents = Math.round(yesPrice * 100);
  const noCents = 100 - yesCents;
  const noPrice = noCents / 100;
  const selectedLimitPrice = outcome === "yes" ? yesPrice : noPrice;
  const inputValue = Number(tradeInput || "0");
  const isMarketBuy = orderType === "market" && action === "buy";
  const averagePrice = selectedLimitPrice;
  const estimatedShares = isMarketBuy ? Math.max(0, inputValue / Math.max(averagePrice, 0.01)) : inputValue;
  const estimatedCost = isMarketBuy ? Math.max(0, inputValue) : Math.max(0, estimatedShares * averagePrice);
  const payoutIfWin = Math.max(0, estimatedShares - estimatedCost);

  const resolvedImageURL = useMemo(() => {
    const raw = metadata?.image || metadata?.image_url || market?.image_url || "";
    return normalizeAssetURL(raw);
  }, [metadata?.image, metadata?.image_url, market?.image_url]);

  const closeTime = market?.close_time || metadata?.close_time || "";
  const settleTime = market?.resolve_after_time || metadata?.resolve_after_time || metadata?.settle_time || "";
  const claimDeadlineTime = market?.claim_deadline_time || metadata?.claim_deadline_time || "";
  const description = buildDescription(market, metadata);

  if (loading) return <div className="mx-auto max-w-6xl px-4 py-10">Loading market...</div>;
  if (!market) return <div className="mx-auto max-w-6xl px-4 py-10">Market not found.</div>;

  const handlePlaceOrder = async () => {
    if (orderType === "limit") {
      const expiryDate = parseLocalDateTime(expiryInput);
      if (!expiryDate) {
        toast.error("Please choose a valid expiry time");
        return;
      }
      if (expiryDate.getTime() <= Date.now()) {
        toast.error("Expiry time must be in the future");
        return;
      }
    }
    const accepted = await placeOrder({
      market,
      action,
      outcome,
      orderType,
      amount: tradeInput,
      limitPrice: selectedLimitPrice,
      expireTime: orderType === "limit" ? localDateTimeToISOString(expiryInput) : undefined,
      onAccepted: (payload) => setLastOrderID(payload.order_id),
    });
    if (accepted) {
      await Promise.all([syncTradingBalance(), syncPosition()]);
    }
  };

  return (
    <div className="min-h-screen bg-zinc-50 dark:bg-zinc-950">
      <main className="mx-auto max-w-[1280px] px-4 py-8 sm:px-6 lg:px-8">
        <Link href="/" className="text-sm font-medium text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100">
          ← Back to markets
        </Link>

        <div className="mt-6 grid gap-8 xl:grid-cols-[1.58fr_0.62fr]">
          <div className="space-y-6">
            <section className="overflow-hidden rounded-[28px] border border-zinc-200 bg-white shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
              <div className="px-6 py-5 sm:px-8">
                <h1 className="text-3xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">{market.title}</h1>
                <div className="mt-4 flex flex-wrap gap-x-6 gap-y-2 text-sm text-zinc-500 dark:text-zinc-400">
                  <span>Close: {formatTime(closeTime)}</span>
                  <span>Settle: {settleTime ? formatTime(settleTime) : "Follows market rules"}</span>
                  <span>Claim by: {claimDeadlineTime ? formatTime(claimDeadlineTime) : "N/A"}</span>
                  <span>Mode: {market.resolution.mode === "pyth" ? "Pyth Oracle" : "Creator Resolved"}</span>
                  <span>Collateral: {market.collateral_mint ? `${market.collateral_mint.slice(0, 8)}...${market.collateral_mint.slice(-6)}` : "N/A"}</span>
                </div>
              </div>

              <div className="border-t border-zinc-200 px-4 py-4 dark:border-zinc-800 sm:px-8">
                {resolvedImageURL ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img
                    src={resolvedImageURL}
                    alt={market.title}
                    className="h-44 w-full rounded-2xl object-cover sm:h-52"
                  />
                ) : (
                  <div className="flex h-44 w-full items-center justify-center rounded-2xl bg-zinc-100 text-sm text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400 sm:h-52">
                    No image in metadata
                  </div>
                )}
              </div>
            </section>

            <PriceChart market={market} outcome={outcome} points={marketFeed.priceHistory} socketState={marketFeed.socketState} loading={marketFeed.loading} />
            <Orderbook outcome={outcome} snapshot={marketFeed.orderbook} socketState={marketFeed.socketState} loading={marketFeed.loading} />
            <UserMarketTabs
              marketId={market.market_id}
              refreshKey={lastOrderID}
              trades={marketFeed.trades}
              publicSocketState={marketFeed.socketState}
              publicLoading={marketFeed.loading}
              matchingEnabled={marketFeed.orderbook.matching_enabled}
            />

            <section className="rounded-[28px] border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
              <h2 className="text-2xl font-semibold text-zinc-900 dark:text-zinc-100">Rules</h2>
              <p className="mt-3 whitespace-pre-wrap text-base leading-7 text-zinc-700 dark:text-zinc-200">{description}</p>
              <dl className="mt-6 grid gap-3 rounded-2xl bg-zinc-50 p-4 text-sm dark:bg-zinc-950/60 sm:grid-cols-2">
                <div>
                  <dt className="text-zinc-500 dark:text-zinc-400">Metadata CID</dt>
                  <dd className="mt-1 break-all font-mono text-zinc-900 dark:text-zinc-100">{market.metadata_cid || "N/A"}</dd>
                </div>
                <div>
                  <dt className="text-zinc-500 dark:text-zinc-400">Claim deadline</dt>
                  <dd className="mt-1 text-zinc-900 dark:text-zinc-100">{claimDeadlineTime ? formatTime(claimDeadlineTime) : "N/A"}</dd>
                </div>
                {market.resolution.mode === "creator" ? (
                  <div>
                    <dt className="text-zinc-500 dark:text-zinc-400">Resolution authority</dt>
                    <dd className="mt-1 break-all font-mono text-zinc-900 dark:text-zinc-100">{market.resolution.authority || "N/A"}</dd>
                  </div>
                ) : (
                  <>
                    <div>
                      <dt className="text-zinc-500 dark:text-zinc-400">Oracle feed</dt>
                      <dd className="mt-1 break-all font-mono text-zinc-900 dark:text-zinc-100">{market.resolution.oracle_feed || "N/A"}</dd>
                    </div>
                    <div>
                      <dt className="text-zinc-500 dark:text-zinc-400">Oracle rule</dt>
                      <dd className="mt-1 text-zinc-900 dark:text-zinc-100">
                        {(market.resolution.oracle_condition || "").toUpperCase()} {market.resolution.oracle_target_price ?? "N/A"}
                      </dd>
                    </div>
                  </>
                )}
              </dl>
            </section>
          </div>

          <aside className="xl:sticky xl:top-6 xl:h-fit">
            <div className="overflow-hidden rounded-[22px] border border-zinc-800 bg-[#0f1118] text-white shadow-[0_12px_32px_rgba(0,0,0,0.35)]">
              <div className="flex items-center justify-between border-b border-white/10 px-3 py-2.5">
                <div className="inline-flex rounded-full bg-white/10 p-1">
                  <button
                    className={`rounded-full px-3.5 py-1.5 text-sm font-semibold transition ${action === "buy" ? "bg-white/20 text-white" : "text-zinc-400"}`}
                    onClick={() => setAction("buy")}
                  >
                    Buy
                  </button>
                  <button
                    className={`rounded-full px-3.5 py-1.5 text-sm font-semibold transition ${action === "sell" ? "bg-white/20 text-white" : "text-zinc-400"}`}
                    onClick={() => setAction("sell")}
                  >
                    Sell
                  </button>
                </div>

                <div className="relative">
                  <button
                    className="inline-flex items-center gap-2 text-sm font-semibold"
                    onClick={() => setShowOrderTypeMenu((value) => !value)}
                  >
                    {orderTypeLabel(orderType)}
                    <span className={`text-sm transition ${showOrderTypeMenu ? "rotate-180" : ""}`}>⌄</span>
                  </button>
                  {showOrderTypeMenu ? (
                    <div className="absolute right-0 z-10 mt-2 w-44 rounded-2xl border border-white/15 bg-[#232734] p-2 shadow-xl">
                      {(["market", "limit", "split", "merge"] as const).map((value) => (
                        <button
                          key={value}
                          className={`block w-full rounded-xl px-3 py-2 text-left text-base ${orderType === value ? "bg-white/10 text-white" : "text-zinc-200 hover:bg-white/5"}`}
                          onClick={() => {
                            setOrderType(value);
                            setShowOrderTypeMenu(false);
                          }}
                        >
                          {orderTypeLabel(value)}
                        </button>
                      ))}
                    </div>
                  ) : null}
                </div>
              </div>

              <div className="space-y-3 px-3 py-3">
                <div className="grid grid-cols-2 gap-2.5">
                  <button
                    className={`rounded-2xl border p-2.5 text-center ${outcome === "yes" ? "border-blue-400 bg-blue-500/10" : "border-white/10 bg-white/5"}`}
                    onClick={() => setOutcome("yes")}
                  >
                    <div className="text-lg font-semibold text-blue-300">Yes {yesCents}¢</div>
                  </button>
                  <button
                    className={`rounded-2xl border p-2.5 text-center ${outcome === "no" ? "border-blue-400 bg-blue-500/10" : "border-white/10 bg-white/5"}`}
                    onClick={() => setOutcome("no")}
                  >
                    <div className="text-lg font-semibold text-zinc-200">No {noCents}¢</div>
                  </button>
                </div>

                {orderType === "limit" ? (
                  <div>
                    <label className="mb-1.5 block text-sm font-semibold text-zinc-200">Limit Price</label>
                    <div className="flex items-center justify-between rounded-2xl border border-white/15 bg-white/5 px-2.5 py-1.5 text-lg">
                      <button
                        className="rounded-lg px-2.5 py-1 text-zinc-300 hover:bg-white/10"
                        onClick={() => setLimitPrice((v) => clampCent(outcome === "yes" ? v - 0.01 : v + 0.01))}
                      >
                        −
                      </button>
                      <span className="font-semibold">{Math.round(selectedLimitPrice * 100)}¢</span>
                      <button
                        className="rounded-lg px-2.5 py-1 text-zinc-300 hover:bg-white/10"
                        onClick={() => setLimitPrice((v) => clampCent(outcome === "yes" ? v + 0.01 : v - 0.01))}
                      >
                        +
                      </button>
                    </div>
                  </div>
                ) : null}

                <div>
                  <label className="mb-1.5 block text-sm font-semibold text-zinc-200">{isMarketBuy ? "Amount (USDC)" : "Shares"}</label>
                  <input
                    className="w-full rounded-2xl border border-white/15 bg-white/5 px-3 py-1.5 text-right text-xl font-semibold outline-none placeholder:text-zinc-500"
                    placeholder="0"
                    value={tradeInput}
                    onChange={(event) => setTradeInput(event.target.value)}
                    inputMode="decimal"
                  />
                  <div className="mt-1.5 grid grid-cols-5 gap-1.5 text-[11px]">
                    {(isMarketBuy ? [-10, -1, +1, +10, +25] : [-100, -10, +10, +100, +200]).map((step) => (
                      <button
                        key={step}
                        className="rounded-xl border border-white/15 bg-white/5 py-1 font-semibold text-zinc-200"
                        onClick={() => setTradeInput((v) => shiftShare(v, step))}
                      >
                        {step > 0 ? `+${step}` : step}
                      </button>
                    ))}
                  </div>
                </div>

                <div className="rounded-2xl border border-white/10 bg-white/5 p-3">
                  <div className="flex items-center justify-between">
                    <span className="text-sm font-semibold text-zinc-100">Market Position</span>
                    <span className="text-[10px] uppercase tracking-[0.2em] text-zinc-500">
                      {positionLoading ? "Loading" : "Projected"}
                    </span>
                  </div>
                  {!user ? (
                    <p className="mt-2 text-sm text-zinc-400">Connect your wallet to inspect YES / NO balances for this market.</p>
                  ) : (
                    <div className="mt-2.5 grid gap-2 sm:grid-cols-2">
                      <PositionColumn
                        title="YES"
                        free={formatLots(position?.yes_free_lots)}
                        locked={formatLots(position?.yes_locked_lots)}
                        pending={formatLots(position?.yes_pending_lots)}
                      />
                      <PositionColumn
                        title="NO"
                        free={formatLots(position?.no_free_lots)}
                        locked={formatLots(position?.no_locked_lots)}
                        pending={formatLots(position?.no_pending_lots)}
                      />
                    </div>
                  )}
                </div>

                {orderType === "limit" ? (
                  <div className="rounded-2xl border border-white/10 p-3">
                    <span className="text-sm font-semibold text-zinc-200">Expiration</span>
                    <input
                      className="mt-2.5 w-full rounded-xl border border-white/15 bg-white/5 px-3 py-1.5 text-sm text-zinc-100 outline-none"
                      max="2099-12-31T23:59:59"
                      min={currentLocalDateTime()}
                      step={1}
                      type="datetime-local"
                      value={expiryInput}
                      onChange={(event) => setExpiryInput(event.target.value)}
                    />
                    <p className="mt-1.5 text-[11px] leading-4 text-zinc-500">Local time, accurate to seconds. Latest allowed date is 2099-12-31 23:59:59.</p>
                  </div>
                ) : null}

                <div className="rounded-2xl border border-white/10 bg-white/5 p-3 text-sm">
                  <SummaryRow label="Average price" value={`${Math.round(averagePrice * 100)}¢`} />
                  <SummaryRow label={isMarketBuy ? "You pay" : "Estimated cost"} value={`$${estimatedCost.toFixed(2)}`} />
                  <SummaryRow label="Estimated shares" value={estimatedShares.toFixed(2)} />
                  <SummaryRow label={`Payout if ${outcome.toUpperCase()} wins`} value={`$${payoutIfWin.toFixed(2)}`} highlight />
                </div>

                <button
                  className={`w-full rounded-2xl px-4 py-2.5 text-base font-semibold text-white shadow-lg ${
                    action === "buy" ? "bg-emerald-500 hover:bg-emerald-400" : "bg-rose-500 hover:bg-rose-400"
                  } disabled:opacity-50`}
                  disabled={tradeLoading || !tradeInput}
                  onClick={() => {
                    void handlePlaceOrder();
                  }}
                >
                  {tradeLoading ? "Submitting..." : `${action === "buy" ? "Buy" : "Sell"} ${outcome.toUpperCase()}`}
                </button>

                {lastOrderID ? (
                  <button
                    className="w-full rounded-2xl border border-white/20 px-4 py-2.5 text-base font-medium text-zinc-100 hover:bg-white/10"
                    onClick={async () => {
                      const token = await getAccessToken();
                      if (!token) {
                        toast.error("Missing identity token");
                        return;
                      }
                      try {
                        await api.delete(`/orders/${lastOrderID}`, {
                          params: { market_id: market.market_id },
                          headers: { Authorization: `Bearer ${token}` },
                        });
                        toast.success("Cancel command accepted", { description: lastOrderID });
                        await Promise.all([syncTradingBalance(), syncPosition()]);
                      } catch (error: unknown) {
                        const response =
                          typeof error === "object" && error !== null && "response" in error
                            ? (error as { response?: { data?: { message?: string } } }).response
                            : undefined;
                        toast.error(response?.data?.message || "Cancel failed");
                      }
                    }}
                  >
                    Cancel last order
                  </button>
                ) : null}

                {!user ? (
                  <button
                    className="w-full rounded-2xl border border-white/20 px-4 py-2.5 text-base font-medium text-zinc-100 hover:bg-white/10"
                    onClick={login}
                  >
                    Connect wallet to trade
                  </button>
                ) : null}
              </div>
            </div>
          </aside>
        </div>
      </main>
    </div>
  );
}

const SummaryRow = ({ label, value, highlight = false }: { label: string; value: string; highlight?: boolean }) => (
  <div className="flex items-center justify-between py-0.5 text-[13px]">
    <span className="text-zinc-400">{label}</span>
    <span className={highlight ? "font-semibold text-emerald-400" : "font-semibold text-zinc-100"}>{value}</span>
  </div>
);

const PositionColumn = ({
  title,
  free,
  locked,
  pending,
}: {
  title: string;
  free: string;
  locked: string;
  pending: string;
}) => (
  <div className="rounded-2xl border border-white/10 bg-[#171b25] p-2.5 text-sm">
    <div className="text-xs uppercase tracking-[0.2em] text-zinc-500">{title}</div>
    <div className="mt-2 space-y-1">
      <SummaryRow label="Available" value={free} />
      <SummaryRow label="Locked" value={locked} />
      <SummaryRow label="Pending" value={pending} />
    </div>
  </div>
);

function orderTypeLabel(value: TradeOrderType): string {
  if (value === "market") return "Market";
  if (value === "limit") return "Limit";
  if (value === "split") return "Split";
  return "Merge";
}

function clampCent(value: number): number {
  if (!Number.isFinite(value)) return 0.5;
  return Math.max(0.01, Math.min(0.99, Math.round(value * 100) / 100));
}

function shiftShare(raw: string, delta: number): string {
  const now = Number(raw || "0");
  if (!Number.isFinite(now)) return Math.max(0, delta).toString();
  return Math.max(0, now + delta).toString();
}

function defaultLimitExpiryLocal(): string {
  const now = new Date();
  now.setSeconds(now.getSeconds() + 23 * 60 * 60);
  return formatDateTimeLocal(now);
}

function currentLocalDateTime(): string {
  return formatDateTimeLocal(new Date());
}

function formatDateTimeLocal(value: Date): string {
  const year = value.getFullYear().toString().padStart(4, "0");
  const month = (value.getMonth() + 1).toString().padStart(2, "0");
  const day = value.getDate().toString().padStart(2, "0");
  const hours = value.getHours().toString().padStart(2, "0");
  const minutes = value.getMinutes().toString().padStart(2, "0");
  const seconds = value.getSeconds().toString().padStart(2, "0");
  return `${year}-${month}-${day}T${hours}:${minutes}:${seconds}`;
}

function parseLocalDateTime(value: string): Date | null {
  if (!value) return null;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return null;
  }
  return parsed;
}

function localDateTimeToISOString(value: string): string {
  const parsed = parseLocalDateTime(value);
  return parsed ? parsed.toISOString() : "";
}

function formatTime(value: string): string {
  if (!value) return "N/A";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString();
}

function normalizeAssetURL(raw: string): string {
  const value = (raw || "").trim();
  if (!value) return "";
  if (value.startsWith("ipfs://")) {
    const path = value.slice("ipfs://".length).replace(/^ipfs\//, "");
    return `https://ipfs.io/ipfs/${path}`;
  }
  return value;
}

function buildDescription(market: Market | null, metadata: MarketMetadataDoc | null): string {
  if (metadata?.rules) {
    if (Array.isArray(metadata.rules)) return metadata.rules.join("\n");
    return metadata.rules;
  }
  if (metadata?.description) return metadata.description;
  if (market?.description) return market.description;
  return "No rules provided.";
}

function formatLots(raw?: string): string {
  const parsed = Number(raw || "0");
  if (!Number.isFinite(parsed)) {
    return "0.00";
  }
  return (parsed / 100).toFixed(2);
}
