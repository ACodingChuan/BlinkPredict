import { Buffer } from "buffer";
import { useState } from "react";
import api from "@/app/utils/axiosInstance";
import { Market, OrderbookSnapshot, PlaceOrderCommandResponse, TransactionEnvelope } from "@/types/market";
import { toast } from "sonner";
import { usePrivy } from "@/lib/auth-client";
import { useSignAndSendTransaction } from "@/hooks/useWalletTransactions";
import { useUSDCStore } from "@/store/usdcStore";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
import {
  buildOrderIntent,
  encodeAmountToUnits,
  encodePriceToTick,
  type BuildOrderIntentParams,
} from "@/lib/order-signature";
import { PublicKey } from "@solana/web3.js";
import { generateClientSnowflake } from "@/lib/snowflake";

interface TradeParams {
  market: Market;
  action: "buy" | "sell";
  outcome: "yes" | "no";
  orderType: "market" | "limit" | "split" | "merge" | "claim";
  amount: string;
  limitPrice: number;
  expireTime?: string;
  onAccepted?: (payload: PlaceOrderCommandResponse) => void;
}

export const useTrading = () => {
  const [loading, setLoading] = useState(false);
  const { getAccessToken } = usePrivy();
  const { signAndSendTransaction } = useSignAndSendTransaction();
  const { wallet, walletAddress } = useCurrentSolanaWallet();
  const syncBalance = useUSDCStore((state) => state.syncBalance);

  const triggerBalanceSync = () => {
    if (walletAddress) {
      void syncBalance(walletAddress);
    }
  };

  const maybeSendTransaction = async (payload: TransactionEnvelope) => {
    if (!payload.tx_message) {
      toast.message(payload.message);
      return true;
    }

    if (!wallet) {
      toast.error("No wallet connected");
      return false;
    }

    const raw = Buffer.from(payload.tx_message, "base64");
    await signAndSendTransaction({
      transaction: new Uint8Array(raw),
    });
    toast.success(payload.message || "Transaction submitted");
    return true;
  };

  const getAuthToken = async (): Promise<string | null> => {
    const token = await getAccessToken();
    if (!token) {
      toast.error("Authentication required");
      return null;
    }
    return token;
  };

  const signOrderMessage = async (message: Uint8Array) => {
    if (!wallet?.signMessage) {
      toast.error("No wallet available for signing");
      return null;
    }

    const signedOutput = await wallet.signMessage(message);
    return Buffer.from(signedOutput).toString("base64");
  };

  const placeOrder = async ({ market, action, outcome, orderType, amount, limitPrice, expireTime, onAccepted }: TradeParams) => {
    setLoading(true);
    try {
      if (orderType === "split" || orderType === "merge") {
        const token = await getAuthToken();
        if (!token) return false;
        const endpoint = orderType === "split" ? "/orders/split" : "/orders/merge";
        const { data } = await api.post<TransactionEnvelope>(
          endpoint,
          {
            market_id: market.market_id,
            collateral_mint: market.collateral_mint,
            amount: Math.floor(Number(amount || "0") * 1_000_000),
          },
          { headers: { Authorization: `Bearer ${token}` } },
        );
        const ok = await maybeSendTransaction(data);
        if (ok) triggerBalanceSync();
        return ok;
      }

      if (orderType === "claim") {
        const token = await getAuthToken();
        if (!token) return false;
        const { data } = await api.post<TransactionEnvelope>(
          "/claims",
          { market_id: market.market_id, collateral_mint: market.collateral_mint, amount: 0 },
          { headers: { Authorization: `Bearer ${token}` } },
        );
        const ok = await maybeSendTransaction(data);
        if (ok) triggerBalanceSync();
        return ok;
      }

      const parsedAmount = Number(amount || "0");
      if (parsedAmount <= 0) {
        toast.error("Amount must be greater than 0");
        return false;
      }

      if (orderType === "limit") {
        if (!toPriceTick(limitPrice)) {
          toast.error("Limit price must be between 0.01 and 0.99");
          return false;
        }
        if (!expireTime) {
          toast.error("Limit order expire time is required");
          return false;
        }
      }

      const token = await getAuthToken();
      if (!token) return false;

      if (!walletAddress) {
        toast.error("No Solana wallet connected");
        return false;
      }

      let expiryTs = 0;
      if (orderType === "limit" && expireTime) {
        expiryTs = Math.floor(new Date(expireTime).getTime() / 1000);
      }

      const programId = process.env.NEXT_PUBLIC_PROGRAM_ID;
      if (!programId) {
        toast.error("Missing NEXT_PUBLIC_PROGRAM_ID");
        return false;
      }
      if (!market.market_pda) {
        toast.error("Missing market PDA");
        return false;
      }

      const limitPriceTick =
        orderType === "market"
          ? await resolveMarketProtectionTick(market.market_id, action, outcome)
          : toPriceTick(limitPrice);
      if (!limitPriceTick) {
        toast.error(orderType === "market" ? "Unable to derive market protection price from orderbook" : "Limit price must be between 0.01 and 0.99");
        return false;
      }
      const totalAmountUnits = encodeAmountToUnits(parsedAmount);

      const buildParams: BuildOrderIntentParams = {
        programId: new PublicKey(programId),
        market: new PublicKey(market.market_pda),
        user: new PublicKey(walletAddress),
        side: action,
        outcome,
        orderType: orderType === "limit" ? "limit" : "market",
        limitPrice: limitPriceTick,
        totalAmount: totalAmountUnits,
        expiryTs,
      };

      const { intent, signableMessage } = buildOrderIntent(buildParams);
      const signature = await signOrderMessage(signableMessage);
      if (!signature) {
        return false;
      }
      const nonce = intent.nonce.toString();
      const idempotencyKey = generateClientSnowflake();
      const traceId = generateClientSnowflake();

      const { data } = await api.post<PlaceOrderCommandResponse & { code?: string }>(
        "/orders",
        {
          version: intent.version,
          program_id: programId,
          market: market.market_pda,
          user: walletAddress,
          side: action,
          outcome,
          order_type: orderType,
          limit_price: limitPriceTick,
          total_amount: totalAmountUnits,
          expiry_ts: expiryTs,
          nonce,
          signature,
        },
        { headers: { Authorization: `Bearer ${token}`, "Idempotency-Key": idempotencyKey, "X-Trace-Id": traceId } },
      );

      if (data.code === "command_bus_not_configured") {
        toast.error("Command bus is not configured");
        return false;
      }

      toast.success("Order command accepted", {
        description: `command_id: ${data.command_id}`,
      });
      onAccepted?.(data);
      await refreshTradingBalance();
      triggerBalanceSync();
      return true;
    } catch (error: unknown) {
      const response = typeof error === "object" && error !== null && "response" in error ? (error as { response?: { data?: { message?: string; code?: string } } }).response : undefined;
      const message = response?.data?.message || (error instanceof Error ? error.message : "Request failed");
      if (response?.data?.code === "command_bus_not_configured") {
        toast.error("Command bus is not configured", { description: message });
        return false;
      }
      toast.error(message);
      return false;
    } finally {
      setLoading(false);
    }
  };

  const refreshTradingBalance = async () => {
    if (!walletAddress) return;

    try {
      await syncBalance(walletAddress);
    } catch (error) {
      console.error("Failed to refresh trading balance:", error);
    }
  };

  return { placeOrder, loading, refreshTradingBalance };
};
function toPriceTick(price: number): number | undefined {
  if (!Number.isFinite(price)) {
    return undefined;
  }
  const tick = encodePriceToTick(price);
  if (tick < 1 || tick > 99) {
    return undefined;
  }
  return tick;
}

async function resolveMarketProtectionTick(
  marketId: string,
  action: "buy" | "sell",
  outcome: "yes" | "no",
): Promise<number | undefined> {
  try {
    const { data } = await api.get<OrderbookSnapshot>(`/orderbook/${marketId}`);
    return deriveMarketProtectionTick(data, action, outcome);
  } catch (error) {
    console.error("Failed to resolve market protection tick", error);
    return undefined;
  }
}

function deriveMarketProtectionTick(
  snapshot: OrderbookSnapshot,
  action: "buy" | "sell",
  outcome: "yes" | "no",
): number | undefined {
  const bestBid = toExistingTick(snapshot.best_bid_price ?? snapshot.bids[0]?.price);
  const bestAsk = toExistingTick(snapshot.best_ask_price ?? snapshot.asks[0]?.price);

  if (action === "buy" && outcome === "yes") {
    return bestAsk ? boundTick(bestAsk + 1) : undefined;
  }
  if (action === "sell" && outcome === "yes") {
    return bestBid ? boundTick(bestBid - 1) : undefined;
  }
  if (action === "buy" && outcome === "no") {
    return bestBid ? boundTick(101 - bestBid) : undefined;
  }
  if (action === "sell" && outcome === "no") {
    return bestAsk ? boundTick(99 - bestAsk) : undefined;
  }
  return undefined;
}

function toExistingTick(value?: string): number | undefined {
  const parsed = Number(value || "");
  if (!Number.isFinite(parsed)) {
    return undefined;
  }
  return clampTick(Math.round(parsed));
}

function clampTick(tick: number): number | undefined {
  if (!Number.isFinite(tick)) {
    return undefined;
  }
  const rounded = Math.round(tick);
  if (rounded < 1 || rounded > 99) {
    return undefined;
  }
  return rounded;
}

function boundTick(tick: number): number {
  return Math.max(1, Math.min(99, Math.round(tick)));
}
