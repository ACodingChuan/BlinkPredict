"use client";

import { useEffect, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { RecentTrades } from "./recent-trades";
import { UserOpenOrders } from "./user-open-orders";
import { TradeItem } from "@/types/market";

type ReadyStatus = {
  writer: string;
  matcher: string;
  pusher: string;
  settlement: string;
  gateway_write_ready: boolean;
};

export const UserMarketTabs = ({
  marketId,
  refreshKey,
  trades,
  publicSocketState,
  publicLoading,
  matchingEnabled,
}: {
  marketId: string;
  refreshKey?: string;
  trades: TradeItem[];
  publicSocketState: "connecting" | "live" | "offline";
  publicLoading: boolean;
  matchingEnabled: boolean;
}) => {
  const [ready, setReady] = useState<ReadyStatus | null>(null);

  useEffect(() => {
    let active = true;

    const loadReady = async () => {
      try {
        const { data } = await api.get<ReadyStatus>("/ready");
        if (!active) return;
        setReady(data);
      } catch (error) {
        console.error("load ready status failed", error);
      }
    };

    void loadReady();
    const timer = setInterval(() => {
      void loadReady();
    }, 15000);

    return () => {
      active = false;
      clearInterval(timer);
    };
  }, []);

  return (
    <section className="space-y-6 rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div className="rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-4 dark:border-zinc-800 dark:bg-zinc-950">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Pusher Status</h3>
            <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">Public orderbook / trades use websocket pusher hot stream; private open orders use plain HTTP polling.</p>
          </div>
          <span className={`rounded-full px-3 py-1 text-xs font-semibold ${ready?.pusher === "ready" ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300" : "bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200"}`}>
            pusher {ready?.pusher || "unknown"}
          </span>
        </div>
        <div className="mt-4 grid gap-3 sm:grid-cols-4">
          <StatusCard label="Writer" value={ready?.writer || "unknown"} />
          <StatusCard label="Matcher" value={ready?.matcher || "unknown"} />
          <StatusCard label="Pusher" value={ready?.pusher || "unknown"} />
          <StatusCard label="Gateway" value={ready?.gateway_write_ready ? "ready" : "not ready"} />
        </div>
      </div>
      <div>
        <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Your Open Orders</h3>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">Cancel sends `cmd.order.cancel.v1` to NATS through gateway. UI refresh is pure HTTP polling.</p>
        <div className="mt-4">
          <UserOpenOrders key={marketId} marketId={marketId} refreshKey={refreshKey} />
        </div>
      </div>
      <div>
        <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Recent Trades</h3>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">Initialized by market snapshot, then only updated by pusher websocket deltas.</p>
        <div className="mt-4">
          <RecentTrades trades={trades} loading={publicLoading} matchingEnabled={matchingEnabled} socketState={publicSocketState} />
        </div>
      </div>
    </section>
  );
};

const StatusCard = ({ label, value }: { label: string; value: string }) => (
  <div className="rounded-2xl border border-zinc-200 bg-white px-3 py-3 text-sm dark:border-zinc-800 dark:bg-zinc-900">
    <div className="text-xs uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">{label}</div>
    <div className="mt-2 font-semibold capitalize text-zinc-900 dark:text-zinc-100">{value}</div>
  </div>
);
