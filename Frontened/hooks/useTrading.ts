import { Buffer } from "buffer";
import { useState } from "react";
import api from "@/app/utils/axiosInstance";
import { Market, TransactionEnvelope } from "@/types/market";
import { toast } from "sonner";
import { useIdentityToken } from "@privy-io/react-auth";
import { useSignAndSendTransaction, useWallets } from "@privy-io/react-auth/solana";

interface TradeParams {
  market: Market;
  action: "buy" | "sell";
  outcome: "yes" | "no";
  orderType: "market" | "limit" | "split" | "merge" | "claim";
  amount: string;
  limitPrice: number;
}

export const useTrading = () => {
  const [loading, setLoading] = useState(false);
  const { identityToken } = useIdentityToken();
  const { signAndSendTransaction } = useSignAndSendTransaction();
  const { wallets } = useWallets();

  const maybeSendTransaction = async (payload: TransactionEnvelope) => {
    if (!payload.tx_message) {
      toast.message(payload.message);
      return true;
    }

    const selectedWallet = wallets[0];
    if (!selectedWallet) {
      toast.error("No wallet connected");
      return false;
    }

    const raw = Buffer.from(payload.tx_message, "base64");
    await signAndSendTransaction({
      transaction: new Uint8Array(raw),
      wallet: selectedWallet,
      chain: "solana:devnet",
    });
    toast.success(payload.message || "Transaction submitted");
    return true;
  };

  const placeOrder = async ({ market, action, outcome, orderType, amount, limitPrice }: TradeParams) => {
    setLoading(true);
    try {
      if (orderType === "split" || orderType === "merge") {
        const endpoint = orderType === "split" ? "/orders/split" : "/orders/merge";
        const { data } = await api.post<TransactionEnvelope>(
          endpoint,
          {
            market_id: market.market_id,
            collateral_mint: market.collateral_mint,
            amount: Math.floor(Number(amount || "0") * 1_000_000),
          },
          { headers: { "privy-id-token": identityToken } },
        );
        return maybeSendTransaction(data);
      }

      if (orderType === "claim") {
        const { data } = await api.post<TransactionEnvelope>(
          "/claims",
          { market_id: market.market_id, collateral_mint: market.collateral_mint, amount: 0 },
          { headers: { "privy-id-token": identityToken } },
        );
        return maybeSendTransaction(data);
      }

      const { data } = await api.post<{ code?: string; message: string }>(
        "/orders",
        {
          market_id: market.market_id,
          collateral_mint: market.collateral_mint,
          side: action === "buy" ? "Bid" : "Ask",
          share: outcome,
          price: limitPrice,
          qty: Number(amount || "0"),
        },
        { headers: { "privy-id-token": identityToken } },
      );

      if (data.code === "matching_not_enabled") {
        toast.message("撮合模块暂未启用", { description: data.message });
        return false;
      }

      toast.success(data.message);
      return true;
    } catch (error: unknown) {
      const response = typeof error === "object" && error !== null && "response" in error ? (error as { response?: { data?: { message?: string; code?: string } } }).response : undefined;
      const message = response?.data?.message || (error instanceof Error ? error.message : "Request failed");
      if (response?.data?.code === "matching_not_enabled") {
        toast.message("撮合模块暂未启用", { description: message });
        return false;
      }
      toast.error(message);
      return false;
    } finally {
      setLoading(false);
    }
  };

  return { placeOrder, loading };
};
