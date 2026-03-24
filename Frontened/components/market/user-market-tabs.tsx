"use client";

import { RecentTrades } from "./recent-trades";
import { UserOpenOrders } from "./user-open-orders";

export const UserMarketTabs = ({ marketId }: { marketId: string }) => {
  return (
    <section className="space-y-6 rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div>
        <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Your Open Orders</h3>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">Cancel sends `cmd.order.cancel.v1` to NATS through gateway.</p>
        <div className="mt-4">
          <UserOpenOrders marketId={marketId} />
        </div>
      </div>
      <div>
        <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Recent Trades</h3>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">Reads from /api/trades/{'{market_id}'} for matcher output verification.</p>
        <div className="mt-4">
          <RecentTrades marketId={marketId} />
        </div>
      </div>
    </section>
  );
};
